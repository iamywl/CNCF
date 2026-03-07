// Jaeger PoC: Badger 스타일 역인덱스(Inverted Index) 시뮬레이션
//
// Jaeger의 Badger 스토리지 백엔드는 span 데이터를 저장할 때
// 다양한 보조 인덱스(secondary index)를 함께 생성한다.
// 이 인덱스들을 통해 서비스명, 오퍼레이션명, 태그, 기간(duration) 등
// 다양한 조건으로 트레이스를 빠르게 검색할 수 있다.
//
// 실제 Jaeger 소스 참조:
//   - internal/storage/v1/badger/spanstore/writer.go: createIndexKey(), WriteSpan()
//   - internal/storage/v1/badger/spanstore/reader.go: mergeJoinIds(), indexSeeksToTraceIDs()
//
// 키 스키마:
//   spanKeyPrefix(0x80) | indexType | indexValue | startTime(8bytes) | traceID(16bytes)
//
// 인덱스 타입:
//   0x81: serviceNameIndexKey     → serviceName
//   0x82: operationNameIndexKey   → serviceName+operationName
//   0x83: tagIndexKey             → serviceName+tagKey+tagValue
//   0x84: durationIndexKey        → durationMicroseconds(BigEndian 8bytes)

package main

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// ============================================================
// 데이터 모델
// ============================================================

// TraceID는 128비트 트레이스 식별자 (Jaeger의 model.TraceID 구조)
type TraceID struct {
	High uint64
	Low  uint64
}

func (t TraceID) String() string {
	if t.High == 0 {
		return fmt.Sprintf("%016x", t.Low)
	}
	return fmt.Sprintf("%016x%016x", t.High, t.Low)
}

// Span은 분산 추적의 단일 작업 단위
type Span struct {
	TraceID       TraceID
	SpanID        uint64
	OperationName string
	ServiceName   string
	StartTime     time.Time
	Duration      time.Duration
	Tags          map[string]string
}

// ============================================================
// 역인덱스 엔진 (Badger 스타일)
// ============================================================

// 인덱스 키 프리픽스 상수 (실제 Jaeger writer.go 참조)
const (
	spanKeyPrefix         byte = 0x80
	indexKeyRange         byte = 0x0F
	serviceNameIndexKey   byte = 0x81
	operationNameIndexKey byte = 0x82
	tagIndexKey           byte = 0x83
	durationIndexKey      byte = 0x84
	sizeOfTraceID              = 16
)

// IndexEntry는 정렬된 KV 스토어의 하나의 인덱스 엔트리
type IndexEntry struct {
	Key   []byte // 인덱스 키 (prefix + value + startTime + traceID)
	Value []byte // 보통 nil (인덱스 엔트리는 키만 사용)
}

// InvertedIndex는 Badger 스타일의 역인덱스 엔진
type InvertedIndex struct {
	// 정렬된 KV 스토어를 시뮬레이션 (실제 Badger는 LSM-tree 기반)
	entries []IndexEntry

	// 원본 span 저장소
	spans map[string]*Span // traceID → Span
}

func NewInvertedIndex() *InvertedIndex {
	return &InvertedIndex{
		entries: make([]IndexEntry, 0),
		spans:   make(map[string]*Span),
	}
}

// createIndexKey는 Jaeger의 실제 인덱스 키 생성 로직을 재현한다.
// KEY: indexKey<indexValue><startTime><traceId>
// 실제 소스: writer.go createIndexKey()
func createIndexKey(indexPrefixKey byte, value []byte, startTime uint64, traceID TraceID) []byte {
	key := make([]byte, 1+len(value)+8+sizeOfTraceID)
	key[0] = (indexPrefixKey & indexKeyRange) | spanKeyPrefix
	pos := len(value) + 1
	copy(key[1:pos], value)
	binary.BigEndian.PutUint64(key[pos:], startTime)
	pos += 8
	binary.BigEndian.PutUint64(key[pos:], traceID.High)
	pos += 8
	binary.BigEndian.PutUint64(key[pos:], traceID.Low)
	return key
}

// WriteSpan은 span을 저장하면서 모든 보조 인덱스를 생성한다.
// 실제 Jaeger writer.go의 WriteSpan()을 재현.
func (idx *InvertedIndex) WriteSpan(span *Span) {
	startTime := uint64(span.StartTime.UnixMicro())

	// 원본 span 저장
	idx.spans[span.TraceID.String()] = span

	// 1. 서비스명 인덱스: serviceName → traceID
	idx.addEntry(createIndexKey(
		serviceNameIndexKey,
		[]byte(span.ServiceName),
		startTime,
		span.TraceID,
	))

	// 2. 오퍼레이션명 인덱스: serviceName+operationName → traceID
	idx.addEntry(createIndexKey(
		operationNameIndexKey,
		[]byte(span.ServiceName+span.OperationName),
		startTime,
		span.TraceID,
	))

	// 3. 태그 인덱스: serviceName+tagKey+tagValue → traceID
	for k, v := range span.Tags {
		idx.addEntry(createIndexKey(
			tagIndexKey,
			[]byte(span.ServiceName+k+v),
			startTime,
			span.TraceID,
		))
	}

	// 4. 기간(Duration) 인덱스: durationMicroseconds → traceID
	durationValue := make([]byte, 8)
	binary.BigEndian.PutUint64(durationValue, uint64(span.Duration.Microseconds()))
	idx.addEntry(createIndexKey(
		durationIndexKey,
		durationValue,
		startTime,
		span.TraceID,
	))

	// 정렬 유지 (실제 Badger는 LSM-tree로 자동 정렬)
	sort.Slice(idx.entries, func(i, j int) bool {
		return string(idx.entries[i].Key) < string(idx.entries[j].Key)
	})
}

func (idx *InvertedIndex) addEntry(key []byte) {
	idx.entries = append(idx.entries, IndexEntry{Key: key, Value: nil})
}

// ============================================================
// 인덱스 스캔 및 필터링
// ============================================================

// scanIndexKeys는 주어진 프리픽스로 인덱스를 스캔하여 traceID 목록을 반환한다.
// 실제 Jaeger reader.go의 scanIndexKeys()를 재현.
func (idx *InvertedIndex) scanIndexKeys(prefix []byte, startTimeMin, startTimeMax uint64) [][]byte {
	results := make([][]byte, 0)

	for _, entry := range idx.entries {
		key := entry.Key

		// 키 길이 검증: prefix + 8(timestamp) + 16(traceID) = prefix_len + 24
		if len(key) != len(prefix)+24 {
			continue
		}

		// 프리픽스 매칭
		timestampStartIndex := len(key) - (sizeOfTraceID + 8)
		if timestampStartIndex < 0 || timestampStartIndex > len(key) {
			continue
		}

		keyPrefix := key[:timestampStartIndex]
		if string(keyPrefix) != string(prefix) {
			continue
		}

		// 시간 범위 필터링
		timestamp := binary.BigEndian.Uint64(key[timestampStartIndex : timestampStartIndex+8])
		if timestamp >= startTimeMin && timestamp <= startTimeMax {
			traceIDBytes := make([]byte, sizeOfTraceID)
			copy(traceIDBytes, key[len(key)-sizeOfTraceID:])
			results = append(results, traceIDBytes)
		}
	}

	return results
}

// bytesToTraceID는 바이트 슬라이스를 TraceID로 변환한다.
// 실제 Jaeger reader.go의 bytesToTraceID() 참조.
func bytesToTraceID(key []byte) TraceID {
	return TraceID{
		High: binary.BigEndian.Uint64(key[:8]),
		Low:  binary.BigEndian.Uint64(key[8:sizeOfTraceID]),
	}
}

// mergeJoinIds는 정렬된 두 traceID 목록의 교집합(AND)을 구한다.
// 실제 Jaeger reader.go의 mergeJoinIds()를 그대로 재현.
// 이 함수가 핵심: 다중 인덱스 결과를 효율적으로 교차시킨다.
func mergeJoinIds(left, right [][]byte) [][]byte {
	allocateSize := len(right)
	if len(left) < allocateSize {
		allocateSize = len(left)
	}

	merged := make([][]byte, 0, allocateSize)

	lMax := len(left) - 1
	rMax := len(right) - 1
	for r, l := 0, 0; r <= rMax && l <= lMax; {
		cmp := compareBytes(left[l], right[r])
		switch {
		case cmp > 0:
			r++
		case cmp < 0:
			l++
		default:
			merged = append(merged, left[l])
			l++
			r++
		}
	}
	return merged
}

func compareBytes(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return len(a) - len(b)
}

// unionIds는 두 traceID 목록의 합집합(OR)을 구한다.
func unionIds(left, right [][]byte) [][]byte {
	seen := make(map[string]struct{})
	result := make([][]byte, 0)

	for _, id := range left {
		key := string(id)
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			result = append(result, id)
		}
	}
	for _, id := range right {
		key := string(id)
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			result = append(result, id)
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return string(result[i]) < string(result[j])
	})
	return result
}

// ============================================================
// 쿼리 인터페이스
// ============================================================

// TraceQuery는 트레이스 검색 조건
type TraceQuery struct {
	ServiceName   string
	OperationName string
	Tags          map[string]string
	StartTimeMin  time.Time
	StartTimeMax  time.Time
	DurationMin   time.Duration
	DurationMax   time.Duration
	Limit         int
}

// FindTraceIDs는 다중 필터 조건으로 traceID를 검색한다.
// 실제 Jaeger reader.go의 FindTraceIDs() 흐름을 재현:
//  1. serviceQueries()로 인덱스 검색 키 목록 생성
//  2. indexSeeksToTraceIDs()로 각 인덱스 스캔 후 merge-join
//  3. durationQueries()로 추가 필터링
func (idx *InvertedIndex) FindTraceIDs(query TraceQuery) []TraceID {
	startTimeMin := uint64(query.StartTimeMin.UnixMicro())
	startTimeMax := uint64(query.StartTimeMax.UnixMicro())

	// 인덱스 검색 키 목록 구성 (실제 serviceQueries() 로직)
	indexSeeks := make([][]byte, 0)

	if query.ServiceName != "" {
		tagQueryUsed := false

		// 태그 인덱스 검색 키
		for k, v := range query.Tags {
			tagSearch := []byte(query.ServiceName + k + v)
			tagSearchKey := make([]byte, 0, len(tagSearch)+1)
			tagSearchKey = append(tagSearchKey, tagIndexKey)
			tagSearchKey = append(tagSearchKey, tagSearch...)
			indexSeeks = append(indexSeeks, tagSearchKey)
			tagQueryUsed = true
		}

		// 오퍼레이션 또는 서비스 인덱스 검색 키
		if query.OperationName != "" {
			opKey := make([]byte, 0, 64)
			opKey = append(opKey, operationNameIndexKey)
			opKey = append(opKey, []byte(query.ServiceName+query.OperationName)...)
			indexSeeks = append(indexSeeks, opKey)
		} else if !tagQueryUsed {
			svcKey := make([]byte, 0, 64)
			svcKey = append(svcKey, serviceNameIndexKey)
			svcKey = append(svcKey, []byte(query.ServiceName)...)
			indexSeeks = append(indexSeeks, svcKey)
		}
	}

	if len(indexSeeks) == 0 {
		return nil
	}

	// 각 인덱스 스캔 후 merge-join (AND 연산)
	// 실제 indexSeeksToTraceIDs() 로직 재현
	var mergedOuter [][]byte

	for i := len(indexSeeks) - 1; i >= 0; i-- {
		indexResults := idx.scanIndexKeys(indexSeeks[i], startTimeMin, startTimeMax)

		// 정렬
		sort.Slice(indexResults, func(k, h int) bool {
			return string(indexResults[k]) < string(indexResults[h])
		})

		// 중복 제거
		deduped := deduplicateTraceIDs(indexResults)

		if mergedOuter == nil {
			mergedOuter = deduped
		} else {
			mergedOuter = mergeJoinIds(mergedOuter, deduped)
		}
	}

	// Duration 필터링 (해시 조인)
	if query.DurationMin > 0 || query.DurationMax > 0 {
		durationMatches := idx.scanDurationIndex(startTimeMin, startTimeMax, query.DurationMin, query.DurationMax)
		durationSet := make(map[string]struct{})
		for _, id := range durationMatches {
			durationSet[string(id)] = struct{}{}
		}

		filtered := make([][]byte, 0)
		for _, id := range mergedOuter {
			if _, ok := durationSet[string(id)]; ok {
				filtered = append(filtered, id)
			}
		}
		mergedOuter = filtered
	}

	// 결과 변환 및 제한
	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > len(mergedOuter) {
		limit = len(mergedOuter)
	}

	traceIDs := make([]TraceID, limit)
	for i := 0; i < limit; i++ {
		traceIDs[i] = bytesToTraceID(mergedOuter[i])
	}

	return traceIDs
}

// scanDurationIndex는 기간 범위로 인덱스를 스캔한다.
func (idx *InvertedIndex) scanDurationIndex(startTimeMin, startTimeMax uint64, durMin, durMax time.Duration) [][]byte {
	results := make([][]byte, 0)
	durMinMicro := uint64(durMin.Microseconds())
	durMaxMicro := uint64(durMax.Microseconds())
	if durMax == 0 {
		durMaxMicro = ^uint64(0) // MaxUint64
	}

	for _, entry := range idx.entries {
		key := entry.Key
		if len(key) < 1+8+8+sizeOfTraceID {
			continue
		}
		// Duration 인덱스 키 확인: prefix(0x84 masked) 체크
		if (key[0] & indexKeyRange) != (durationIndexKey & indexKeyRange) {
			continue
		}

		// duration 값 추출 (key[1:9])
		durValue := binary.BigEndian.Uint64(key[1:9])
		if durValue < durMinMicro || durValue > durMaxMicro {
			continue
		}

		// 시간 범위 확인
		timestamp := binary.BigEndian.Uint64(key[9:17])
		if timestamp < startTimeMin || timestamp > startTimeMax {
			continue
		}

		traceIDBytes := make([]byte, sizeOfTraceID)
		copy(traceIDBytes, key[len(key)-sizeOfTraceID:])
		results = append(results, traceIDBytes)
	}

	return results
}

func deduplicateTraceIDs(ids [][]byte) [][]byte {
	if len(ids) == 0 {
		return ids
	}
	result := [][]byte{ids[0]}
	for i := 1; i < len(ids); i++ {
		if string(ids[i]) != string(ids[i-1]) {
			result = append(result, ids[i])
		}
	}
	return result
}

// FullScan은 인덱스 없이 전체 span을 순회하는 풀스캔 방식
func (idx *InvertedIndex) FullScan(query TraceQuery) []TraceID {
	results := make([]TraceID, 0)

	for _, span := range idx.spans {
		if query.ServiceName != "" && span.ServiceName != query.ServiceName {
			continue
		}
		if query.OperationName != "" && span.OperationName != query.OperationName {
			continue
		}
		if !query.StartTimeMin.IsZero() && span.StartTime.Before(query.StartTimeMin) {
			continue
		}
		if !query.StartTimeMax.IsZero() && span.StartTime.After(query.StartTimeMax) {
			continue
		}
		if query.DurationMin > 0 && span.Duration < query.DurationMin {
			continue
		}
		if query.DurationMax > 0 && span.Duration > query.DurationMax {
			continue
		}

		// 태그 매칭
		tagMatch := true
		for k, v := range query.Tags {
			if spanVal, ok := span.Tags[k]; !ok || spanVal != v {
				tagMatch = false
				break
			}
		}
		if !tagMatch {
			continue
		}

		results = append(results, span.TraceID)
	}

	// 제한
	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > len(results) {
		limit = len(results)
	}
	return results[:limit]
}

// ============================================================
// 샘플 데이터 생성
// ============================================================

var (
	services   = []string{"api-gateway", "user-service", "order-service", "payment-service", "inventory-service"}
	operations = map[string][]string{
		"api-gateway":       {"GET /users", "POST /orders", "GET /products", "GET /health"},
		"user-service":      {"GetUser", "CreateUser", "UpdateUser", "DeleteUser"},
		"order-service":     {"CreateOrder", "GetOrder", "ListOrders", "CancelOrder"},
		"payment-service":   {"ProcessPayment", "RefundPayment", "GetPaymentStatus"},
		"inventory-service": {"CheckStock", "ReserveItem", "ReleaseItem"},
	}
	tagKeys   = []string{"http.status_code", "http.method", "error", "env", "region"}
	tagValues = map[string][]string{
		"http.status_code": {"200", "201", "400", "404", "500"},
		"http.method":      {"GET", "POST", "PUT", "DELETE"},
		"error":            {"true", "false"},
		"env":              {"production", "staging", "development"},
		"region":           {"us-east-1", "us-west-2", "eu-west-1", "ap-northeast-1"},
	}
)

func generateSpans(count int) []*Span {
	rng := rand.New(rand.NewSource(42))
	baseTime := time.Now().Add(-1 * time.Hour)
	spans := make([]*Span, count)

	for i := 0; i < count; i++ {
		svc := services[rng.Intn(len(services))]
		ops := operations[svc]
		op := ops[rng.Intn(len(ops))]

		tags := make(map[string]string)
		numTags := rng.Intn(3) + 1
		for t := 0; t < numTags; t++ {
			tk := tagKeys[rng.Intn(len(tagKeys))]
			tv := tagValues[tk]
			tags[tk] = tv[rng.Intn(len(tv))]
		}

		spans[i] = &Span{
			TraceID: TraceID{
				High: 0,
				Low:  uint64(i + 1),
			},
			SpanID:        uint64(rng.Int63()),
			OperationName: op,
			ServiceName:   svc,
			StartTime:     baseTime.Add(time.Duration(rng.Intn(3600)) * time.Second),
			Duration:      time.Duration(rng.Intn(5000)+1) * time.Millisecond,
			Tags:          tags,
		}
	}
	return spans
}

// ============================================================
// 메인: 데모 실행
// ============================================================

func main() {
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("  Jaeger PoC: Badger 스타일 역인덱스(Inverted Index) 시뮬레이션")
	fmt.Println("=" + strings.Repeat("=", 79))

	// ----------------------------------------------------------
	// 1. 데이터 생성 및 인덱스 구축
	// ----------------------------------------------------------
	fmt.Println("\n[1단계] 샘플 데이터 생성 및 인덱스 구축")
	fmt.Println(strings.Repeat("-", 60))

	index := NewInvertedIndex()
	spans := generateSpans(10000)

	startBuild := time.Now()
	for _, span := range spans {
		index.WriteSpan(span)
	}
	buildDuration := time.Since(startBuild)

	fmt.Printf("  span 수: %d\n", len(spans))
	fmt.Printf("  인덱스 엔트리 수: %d\n", len(index.entries))
	fmt.Printf("  인덱스 구축 시간: %v\n", buildDuration)

	// 인덱스 타입별 통계
	typeCounts := make(map[byte]int)
	for _, entry := range index.entries {
		typeCounts[entry.Key[0]]++
	}
	fmt.Println("\n  인덱스 타입별 엔트리 수:")
	typeNames := map[byte]string{
		(serviceNameIndexKey & indexKeyRange) | spanKeyPrefix:   "서비스명 인덱스 (0x81)",
		(operationNameIndexKey & indexKeyRange) | spanKeyPrefix: "오퍼레이션명 인덱스 (0x82)",
		(tagIndexKey & indexKeyRange) | spanKeyPrefix:           "태그 인덱스 (0x83)",
		(durationIndexKey & indexKeyRange) | spanKeyPrefix:      "기간 인덱스 (0x84)",
	}
	for prefix, name := range typeNames {
		fmt.Printf("    %s: %d\n", name, typeCounts[prefix])
	}

	// ----------------------------------------------------------
	// 2. 인덱스 키 구조 시각화
	// ----------------------------------------------------------
	fmt.Println("\n[2단계] 인덱스 키 구조 시각화")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  Badger 인덱스 키 스키마 (실제 Jaeger writer.go createIndexKey()):

  +---------+-----------+------------+-----------+
  | Prefix  |  Value    | StartTime  |  TraceID  |
  | (1byte) | (가변)    | (8 bytes)  | (16 bytes)|
  +---------+-----------+------------+-----------+

  서비스 인덱스:     0x81 | "api-gateway"        | timestamp | traceID
  오퍼레이션 인덱스: 0x82 | "api-gatewayGET /users" | timestamp | traceID
  태그 인덱스:       0x83 | "api-gatewayhttp.status_code200" | timestamp | traceID
  기간 인덱스:       0x84 | duration(8bytes BE)  | timestamp | traceID

  * 모든 키는 BigEndian 바이트 순서로 저장되어 사전순 정렬 가능
  * Badger(LSM-tree)는 키 정렬을 자동으로 유지하므로 범위 스캔이 효율적`)

	// 실제 키 예시 출력
	if len(index.entries) > 0 {
		fmt.Println("\n  실제 인덱스 키 예시 (첫 3개):")
		for i := 0; i < 3 && i < len(index.entries); i++ {
			key := index.entries[i].Key
			fmt.Printf("    [%d] prefix=0x%02X len=%d hex=%X\n", i, key[0], len(key), key[:min(32, len(key))])
		}
	}

	// ----------------------------------------------------------
	// 3. 단일 인덱스 검색
	// ----------------------------------------------------------
	fmt.Println("\n[3단계] 단일 인덱스 검색 (서비스명 기반)")
	fmt.Println(strings.Repeat("-", 60))

	query1 := TraceQuery{
		ServiceName:  "order-service",
		StartTimeMin: time.Now().Add(-2 * time.Hour),
		StartTimeMax: time.Now(),
		Limit:        10,
	}

	startSearch := time.Now()
	results1 := index.FindTraceIDs(query1)
	search1Duration := time.Since(startSearch)

	fmt.Printf("  쿼리: service=%q\n", query1.ServiceName)
	fmt.Printf("  결과 수: %d (limit=%d)\n", len(results1), query1.Limit)
	fmt.Printf("  검색 시간: %v\n", search1Duration)
	if len(results1) > 0 {
		fmt.Printf("  첫 번째 traceID: %s\n", results1[0].String())
	}

	// ----------------------------------------------------------
	// 4. 다중 인덱스 교차(AND) 검색
	// ----------------------------------------------------------
	fmt.Println("\n[4단계] 다중 인덱스 교차(AND) 검색 - merge-join")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  merge-join 알고리즘 (실제 Jaeger reader.go mergeJoinIds()):

    sorted_left:  [A, B, C, D, E]
    sorted_right: [B, D, F]

    l=0,r=0: A < B → l++
    l=1,r=0: B = B → merged=[B], l++, r++
    l=2,r=1: C < D → l++
    l=3,r=1: D = D → merged=[B,D], l++, r++
    l=4,r=2: E < F → l++
    → 결과: [B, D]

  * O(n+m) 시간 복잡도로 효율적인 교집합 연산`)

	query2 := TraceQuery{
		ServiceName:   "api-gateway",
		OperationName: "GET /users",
		Tags: map[string]string{
			"http.status_code": "200",
		},
		StartTimeMin: time.Now().Add(-2 * time.Hour),
		StartTimeMax: time.Now(),
		Limit:        10,
	}

	startSearch2 := time.Now()
	results2 := index.FindTraceIDs(query2)
	search2Duration := time.Since(startSearch2)

	fmt.Printf("\n  쿼리: service=%q, op=%q, tag=http.status_code:200\n",
		query2.ServiceName, query2.OperationName)
	fmt.Printf("  결과 수: %d\n", len(results2))
	fmt.Printf("  검색 시간: %v\n", search2Duration)
	for i, id := range results2 {
		if i >= 5 {
			fmt.Printf("  ... (나머지 %d개 생략)\n", len(results2)-5)
			break
		}
		span := index.spans[id.String()]
		if span != nil {
			fmt.Printf("    [%d] traceID=%s service=%s op=%s tags=%v\n",
				i, id.String(), span.ServiceName, span.OperationName, span.Tags)
		}
	}

	// ----------------------------------------------------------
	// 5. OR 검색 (유니온)
	// ----------------------------------------------------------
	fmt.Println("\n[5단계] 인덱스 유니온(OR) 검색")
	fmt.Println(strings.Repeat("-", 60))

	// 두 서비스의 결과를 합집합
	queryA := TraceQuery{
		ServiceName:  "payment-service",
		StartTimeMin: time.Now().Add(-2 * time.Hour),
		StartTimeMax: time.Now(),
		Limit:        1000,
	}
	queryB := TraceQuery{
		ServiceName:  "inventory-service",
		StartTimeMin: time.Now().Add(-2 * time.Hour),
		StartTimeMax: time.Now(),
		Limit:        1000,
	}

	resultsA := index.FindTraceIDs(queryA)
	resultsB := index.FindTraceIDs(queryB)

	// TraceID → []byte 변환 후 유니온
	bytesA := make([][]byte, len(resultsA))
	for i, id := range resultsA {
		b := make([]byte, sizeOfTraceID)
		binary.BigEndian.PutUint64(b[:8], id.High)
		binary.BigEndian.PutUint64(b[8:], id.Low)
		bytesA[i] = b
	}
	bytesB := make([][]byte, len(resultsB))
	for i, id := range resultsB {
		b := make([]byte, sizeOfTraceID)
		binary.BigEndian.PutUint64(b[:8], id.High)
		binary.BigEndian.PutUint64(b[8:], id.Low)
		bytesB[i] = b
	}

	unionResult := unionIds(bytesA, bytesB)

	fmt.Printf("  payment-service 결과: %d개\n", len(resultsA))
	fmt.Printf("  inventory-service 결과: %d개\n", len(resultsB))
	fmt.Printf("  OR 합집합 결과: %d개\n", len(unionResult))

	// ----------------------------------------------------------
	// 6. Duration 필터 검색
	// ----------------------------------------------------------
	fmt.Println("\n[6단계] Duration 인덱스 필터 검색")
	fmt.Println(strings.Repeat("-", 60))

	query3 := TraceQuery{
		ServiceName:  "order-service",
		DurationMin:  3 * time.Second,
		DurationMax:  5 * time.Second,
		StartTimeMin: time.Now().Add(-2 * time.Hour),
		StartTimeMax: time.Now(),
		Limit:        10,
	}

	startSearch3 := time.Now()
	results3 := index.FindTraceIDs(query3)
	search3Duration := time.Since(startSearch3)

	fmt.Printf("  쿼리: service=%q, duration=[3s, 5s]\n", query3.ServiceName)
	fmt.Printf("  결과 수: %d\n", len(results3))
	fmt.Printf("  검색 시간: %v\n", search3Duration)
	for i, id := range results3 {
		if i >= 5 {
			break
		}
		span := index.spans[id.String()]
		if span != nil {
			fmt.Printf("    [%d] traceID=%s duration=%v\n", i, id.String(), span.Duration)
		}
	}

	// ----------------------------------------------------------
	// 7. 인덱스 검색 vs 풀스캔 성능 비교
	// ----------------------------------------------------------
	fmt.Println("\n[7단계] 인덱스 검색 vs 풀스캔 성능 비교")
	fmt.Println(strings.Repeat("-", 60))

	benchQuery := TraceQuery{
		ServiceName:   "api-gateway",
		OperationName: "POST /orders",
		Tags: map[string]string{
			"http.status_code": "500",
		},
		StartTimeMin: time.Now().Add(-2 * time.Hour),
		StartTimeMax: time.Now(),
		Limit:        100,
	}

	// 인덱스 검색 벤치마크
	iterations := 100
	startIdx := time.Now()
	var indexResults []TraceID
	for i := 0; i < iterations; i++ {
		indexResults = index.FindTraceIDs(benchQuery)
	}
	indexDuration := time.Since(startIdx)

	// 풀스캔 벤치마크
	startFull := time.Now()
	var fullResults []TraceID
	for i := 0; i < iterations; i++ {
		fullResults = index.FullScan(benchQuery)
	}
	fullDuration := time.Since(startFull)

	fmt.Printf("  쿼리: service=%q, op=%q, tag=http.status_code:500\n",
		benchQuery.ServiceName, benchQuery.OperationName)
	fmt.Printf("  span 총 수: %d\n", len(spans))
	fmt.Printf("  반복 횟수: %d\n\n", iterations)

	fmt.Printf("  [인덱스 검색]\n")
	fmt.Printf("    결과 수: %d\n", len(indexResults))
	fmt.Printf("    총 시간: %v\n", indexDuration)
	fmt.Printf("    평균 시간: %v/회\n", indexDuration/time.Duration(iterations))

	fmt.Printf("\n  [풀스캔]\n")
	fmt.Printf("    결과 수: %d\n", len(fullResults))
	fmt.Printf("    총 시간: %v\n", fullDuration)
	fmt.Printf("    평균 시간: %v/회\n", fullDuration/time.Duration(iterations))

	if indexDuration > 0 {
		speedup := float64(fullDuration) / float64(indexDuration)
		fmt.Printf("\n  성능 비교: 인덱스 검색이 풀스캔 대비 약 %.1f배 빠름\n", speedup)
	}

	// ----------------------------------------------------------
	// 8. 실행 계획 시각화
	// ----------------------------------------------------------
	fmt.Println("\n[8단계] 쿼리 실행 계획 시각화")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  실제 Jaeger FindTraceIDs() 실행 흐름:

  ┌──────────────────────────────────────────────────────────┐
  │  FindTraceIDs(service="api-gateway",                     │
  │               op="POST /orders",                         │
  │               tag=http.status_code:500)                  │
  └──────────────┬───────────────────────────────────────────┘
                 │
  ┌──────────────▼───────────────────────────────────────────┐
  │  serviceQueries(): 인덱스 검색 키 생성                     │
  │    [0] opKey  = 0x82 + "api-gatewayPOST /orders"         │
  │    [1] tagKey = 0x83 + "api-gatewayhttp.status_code500"  │
  └──────────────┬───────────────────────────────────────────┘
                 │
  ┌──────────────▼───────────────────────────────────────────┐
  │  indexSeeksToTraceIDs():                                  │
  │                                                           │
  │    [1] scanIndexKeys(tagKey) → traceIDs_tag              │
  │    [0] scanIndexKeys(opKey)  → traceIDs_op               │
  │                                                           │
  │    mergeJoinIds(traceIDs_tag, traceIDs_op)               │
  │    → 교집합(AND) 결과                                     │
  └──────────────┬───────────────────────────────────────────┘
                 │
  ┌──────────────▼───────────────────────────────────────────┐
  │  결과: service AND operation AND tag 모두 만족하는 traceIDs│
  │  → limit 적용 후 반환                                     │
  └──────────────────────────────────────────────────────────┘

  * 역방향(Reverse) 스캔으로 최신 트레이스부터 반환
  * Duration 필터는 해시 조인(hashOuter)으로 추가 필터링`)

	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("  시뮬레이션 완료")
	fmt.Println(strings.Repeat("=", 80))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
