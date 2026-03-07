package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// =============================================================================
// Jaeger Cassandra Duration Index 전략 시뮬레이션
//
// 실제 소스코드 참조:
//   - internal/storage/v1/cassandra/spanstore/writer.go: indexByDuration()
//   - internal/storage/v1/cassandra/spanstore/reader.go: queryByDuration()
//   - docs/adr/001-cassandra-find-traces-duration.md
//
// 핵심 설계:
//   1. durationBucketSize = time.Hour → 1시간 단위 파티션
//   2. 파티션 키: (service_name, operation_name, bucket)
//   3. 클러스터링 컬럼: duration → 범위 필터링 가능
//   4. 스팬 하나당 인덱스 엔트리 2개 생성 (operation_name="" / operation_name=실제값)
//   5. Duration 쿼리는 태그 쿼리와 교차(intersect) 불가
// =============================================================================

const durationBucketSize = time.Hour

// ---------------------------------------------------------------------------
// 데이터 모델
// ---------------------------------------------------------------------------

// TraceID는 분산 추적의 고유 식별자
type TraceID string

// Span은 추적 내 하나의 작업 단위
type Span struct {
	TraceID       TraceID
	SpanID        string
	ServiceName   string
	OperationName string
	StartTime     time.Time
	Duration      time.Duration
	Tags          map[string]string
}

// DurationIndexEntry는 duration_index 테이블의 한 행을 나타냄
// Cassandra CQL:
//
//	INSERT INTO duration_index(service_name, operation_name, bucket, duration, start_time, trace_id)
//	VALUES (?, ?, ?, ?, ?, ?)
type DurationIndexEntry struct {
	ServiceName   string
	OperationName string // "" = 서비스 전체 검색용
	Bucket        time.Time
	Duration      time.Duration
	StartTime     time.Time
	TraceID       TraceID
}

// TagIndexEntry는 tag_index 테이블의 한 행을 나타냄
type TagIndexEntry struct {
	ServiceName string
	TagKey      string
	TagValue    string
	StartTime   time.Time
	TraceID     TraceID
}

// ---------------------------------------------------------------------------
// Cassandra 스토리지 시뮬레이션
// ---------------------------------------------------------------------------

// partitionKey는 duration_index의 파티션 키: (service_name, operation_name, bucket)
type partitionKey struct {
	ServiceName   string
	OperationName string
	Bucket        string // time.Format 문자열
}

// CassandraStore는 Jaeger의 Cassandra 스토리지를 시뮬레이션
type CassandraStore struct {
	// duration_index: 파티션 키 → 엔트리 목록 (duration 기준 정렬)
	durationIndex map[partitionKey][]DurationIndexEntry

	// tag_index: 간단히 리스트로 저장
	tagIndex []TagIndexEntry

	// 통계
	durationIndexWrites int
	tagIndexWrites      int
}

func NewCassandraStore() *CassandraStore {
	return &CassandraStore{
		durationIndex: make(map[partitionKey][]DurationIndexEntry),
	}
}

// ---------------------------------------------------------------------------
// Writer: indexByDuration 시뮬레이션
// 실제 코드: writer.go L229-L243
// ---------------------------------------------------------------------------

// IndexByDuration은 실제 Jaeger의 indexByDuration 함수를 재현
// 핵심: 하나의 스팬에 대해 두 개의 인덱스 엔트리를 생성
//  1. operation_name="" → 서비스 전체에 대한 duration 검색용
//  2. operation_name=실제값 → 특정 오퍼레이션에 대한 duration 검색용
func (s *CassandraStore) IndexByDuration(span Span) {
	// timeBucket := startTime.Round(durationBucketSize)
	timeBucket := span.StartTime.Round(durationBucketSize)

	// indexByOperationName("") — 서비스명만으로 인덱싱
	s.insertDurationIndex(DurationIndexEntry{
		ServiceName:   span.ServiceName,
		OperationName: "",
		Bucket:        timeBucket,
		Duration:      span.Duration,
		StartTime:     span.StartTime,
		TraceID:       span.TraceID,
	})

	// indexByOperationName(span.OperationName) — 서비스명 + 오퍼레이션명으로 인덱싱
	s.insertDurationIndex(DurationIndexEntry{
		ServiceName:   span.ServiceName,
		OperationName: span.OperationName,
		Bucket:        timeBucket,
		Duration:      span.Duration,
		StartTime:     span.StartTime,
		TraceID:       span.TraceID,
	})
}

func (s *CassandraStore) insertDurationIndex(entry DurationIndexEntry) {
	pk := partitionKey{
		ServiceName:   entry.ServiceName,
		OperationName: entry.OperationName,
		Bucket:        entry.Bucket.Format(time.RFC3339),
	}
	s.durationIndex[pk] = append(s.durationIndex[pk], entry)
	s.durationIndexWrites++
}

// IndexByTag는 태그 인덱스에 엔트리를 추가
func (s *CassandraStore) IndexByTag(span Span) {
	for k, v := range span.Tags {
		s.tagIndex = append(s.tagIndex, TagIndexEntry{
			ServiceName: span.ServiceName,
			TagKey:      k,
			TagValue:    v,
			StartTime:   span.StartTime,
			TraceID:     span.TraceID,
		})
		s.tagIndexWrites++
	}
}

// ---------------------------------------------------------------------------
// Reader: queryByDuration 시뮬레이션
// 실제 코드: reader.go L352-L394
// ---------------------------------------------------------------------------

// TraceQueryParameters는 추적 검색 파라미터
type TraceQueryParameters struct {
	ServiceName   string
	OperationName string
	StartTimeMin  time.Time
	StartTimeMax  time.Time
	DurationMin   time.Duration
	DurationMax   time.Duration
	Tags          map[string]string
	NumTraces     int
}

// QueryResult는 쿼리 결과
type QueryResult struct {
	TraceIDs       []TraceID
	BucketsScanned int
	EntriesScanned int
	Error          error
}

// QueryByDuration은 실제 Jaeger의 queryByDuration 함수를 재현
// 핵심: endTime부터 startTime까지 1시간 단위로 역순 반복
func (s *CassandraStore) QueryByDuration(params TraceQueryParameters) QueryResult {
	result := QueryResult{}
	seen := make(map[TraceID]bool)

	// Duration 기본값: max가 0이면 24시간으로 설정
	// 실제 코드: maxDurationMicros = (time.Hour * 24).Nanoseconds() / ...
	maxDuration := 24 * time.Hour
	if params.DurationMax != 0 {
		maxDuration = params.DurationMax
	}
	minDuration := params.DurationMin

	if params.NumTraces == 0 {
		params.NumTraces = 100
	}

	// 시간 버킷 계산 (실제 코드: L366-L367)
	startTimeByHour := params.StartTimeMin.Round(durationBucketSize)
	endTimeByHour := params.StartTimeMax.Round(durationBucketSize)

	// endTime부터 startTime까지 역순으로 버킷 반복 (실제 코드: L369)
	// for timeBucket := endTimeByHour; timeBucket.After(startTimeByHour) || timeBucket.Equal(startTimeByHour);
	//     timeBucket = timeBucket.Add(-1 * durationBucketSize)
	for timeBucket := endTimeByHour; !timeBucket.Before(startTimeByHour); timeBucket = timeBucket.Add(-durationBucketSize) {
		result.BucketsScanned++

		pk := partitionKey{
			ServiceName:   params.ServiceName,
			OperationName: params.OperationName,
			Bucket:        timeBucket.Format(time.RFC3339),
		}

		entries, exists := s.durationIndex[pk]
		if !exists {
			continue
		}

		// Cassandra에서 duration 범위 필터링 (클러스터링 컬럼)
		// WHERE duration > ? AND duration < ?
		for _, entry := range entries {
			result.EntriesScanned++
			if entry.Duration >= minDuration && entry.Duration <= maxDuration {
				if !seen[entry.TraceID] {
					seen[entry.TraceID] = true
					result.TraceIDs = append(result.TraceIDs, entry.TraceID)
					if len(result.TraceIDs) >= params.NumTraces {
						return result
					}
				}
			}
		}
	}

	return result
}

// QueryByTag는 태그 기반 쿼리 (duration 쿼리와는 교차 불가)
func (s *CassandraStore) QueryByTag(serviceName, tagKey, tagValue string, startMin, startMax time.Time) []TraceID {
	seen := make(map[TraceID]bool)
	var results []TraceID
	for _, entry := range s.tagIndex {
		if entry.ServiceName == serviceName &&
			entry.TagKey == tagKey &&
			entry.TagValue == tagValue &&
			!entry.StartTime.Before(startMin) &&
			!entry.StartTime.After(startMax) {
			if !seen[entry.TraceID] {
				seen[entry.TraceID] = true
				results = append(results, entry.TraceID)
			}
		}
	}
	return results
}

// ValidateQuery는 실제 Jaeger의 validateQuery 함수를 재현
// 실제 코드: reader.go L228-L248
func ValidateQuery(p TraceQueryParameters) error {
	if p.StartTimeMin.IsZero() || p.StartTimeMax.IsZero() {
		return fmt.Errorf("start와 end 시간은 필수입니다")
	}
	if p.StartTimeMax.Before(p.StartTimeMin) {
		return fmt.Errorf("시작 시간이 종료 시간보다 큽니다")
	}
	if p.DurationMin != 0 && p.DurationMax != 0 && p.DurationMin > p.DurationMax {
		return fmt.Errorf("최소 duration이 최대 duration보다 큽니다")
	}
	// 핵심 제약: Duration과 태그 쿼리는 동시에 사용할 수 없음
	// 실제 코드: ErrDurationAndTagQueryNotSupported
	if (p.DurationMin != 0 || p.DurationMax != 0) && len(p.Tags) > 0 {
		return fmt.Errorf("duration과 태그를 동시에 쿼리할 수 없습니다 (ErrDurationAndTagQueryNotSupported)")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 시각화 헬퍼
// ---------------------------------------------------------------------------

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 80))
	fmt.Printf(" %s\n", title)
	fmt.Println(strings.Repeat("=", 80))
}

func printSubSeparator(title string) {
	fmt.Printf("\n--- %s ---\n", title)
}

// ---------------------------------------------------------------------------
// 메인: 시뮬레이션 실행
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("Jaeger Cassandra Duration Index 전략 시뮬레이션")
	fmt.Println("참조: internal/storage/v1/cassandra/spanstore/writer.go, reader.go")
	fmt.Println("참조: docs/adr/001-cassandra-find-traces-duration.md")

	store := NewCassandraStore()
	rng := rand.New(rand.NewSource(42))

	// =========================================================================
	// 1단계: 샘플 스팬 데이터 생성 및 인덱싱
	// =========================================================================
	printSeparator("1단계: 스팬 데이터 생성 및 Duration Index 쓰기")

	baseTime := time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)
	services := []string{"api-gateway", "user-service", "order-service"}
	operations := map[string][]string{
		"api-gateway":   {"GET /users", "POST /orders", "GET /health"},
		"user-service":  {"GetUser", "ListUsers", "UpdateUser"},
		"order-service": {"CreateOrder", "GetOrder", "ProcessPayment"},
	}

	var allSpans []Span
	traceCounter := 0

	// 6시간 범위에 걸쳐 스팬 생성 (6개의 시간 버킷)
	for hour := 0; hour < 6; hour++ {
		spansPerHour := 5 + rng.Intn(10)
		for i := 0; i < spansPerHour; i++ {
			traceCounter++
			svc := services[rng.Intn(len(services))]
			ops := operations[svc]
			op := ops[rng.Intn(len(ops))]

			// 다양한 duration 생성 (1ms ~ 5000ms)
			durationMs := 1 + rng.Intn(5000)

			span := Span{
				TraceID:       TraceID(fmt.Sprintf("trace-%04d", traceCounter)),
				SpanID:        fmt.Sprintf("span-%04d", traceCounter),
				ServiceName:   svc,
				OperationName: op,
				StartTime:     baseTime.Add(time.Duration(hour)*time.Hour + time.Duration(rng.Intn(3600))*time.Second),
				Duration:      time.Duration(durationMs) * time.Millisecond,
				Tags: map[string]string{
					"http.status_code": fmt.Sprintf("%d", []int{200, 200, 200, 404, 500}[rng.Intn(5)]),
					"environment":      []string{"prod", "staging"}[rng.Intn(2)],
				},
			}

			allSpans = append(allSpans, span)

			// Duration 인덱스와 태그 인덱스 모두 기록
			store.IndexByDuration(span)
			store.IndexByTag(span)
		}
	}

	fmt.Printf("생성된 스팬 수: %d\n", len(allSpans))
	fmt.Printf("Duration 인덱스 쓰기 횟수: %d (스팬당 2개 엔트리)\n", store.durationIndexWrites)
	fmt.Printf("태그 인덱스 쓰기 횟수: %d\n", store.tagIndexWrites)

	// =========================================================================
	// 2단계: 파티션 분포 분석
	// =========================================================================
	printSeparator("2단계: 파티션 키 분포 분석")

	fmt.Println()
	fmt.Println("Duration Index 파티션 키: (service_name, operation_name, bucket)")
	fmt.Println("Cassandra에서 파티션 키는 동일 노드에 저장 → 파티션 크기가 고르게 분포되어야 함")
	fmt.Println()

	type partitionInfo struct {
		key   partitionKey
		count int
	}
	var partitions []partitionInfo
	for pk, entries := range store.durationIndex {
		partitions = append(partitions, partitionInfo{pk, len(entries)})
	}
	sort.Slice(partitions, func(i, j int) bool {
		if partitions[i].key.ServiceName != partitions[j].key.ServiceName {
			return partitions[i].key.ServiceName < partitions[j].key.ServiceName
		}
		if partitions[i].key.OperationName != partitions[j].key.OperationName {
			return partitions[i].key.OperationName < partitions[j].key.OperationName
		}
		return partitions[i].key.Bucket < partitions[j].key.Bucket
	})

	fmt.Printf("%-18s %-20s %-22s  엔트리수\n", "서비스명", "오퍼레이션명", "버킷(시간)")
	fmt.Println(strings.Repeat("-", 75))

	// 서비스별로 그룹핑하여 일부만 출력
	currentService := ""
	displayCount := 0
	for _, p := range partitions {
		if p.key.ServiceName != currentService {
			currentService = p.key.ServiceName
			displayCount = 0
		}
		displayCount++
		if displayCount <= 3 { // 서비스당 최대 3개만 표시
			opName := p.key.OperationName
			if opName == "" {
				opName = "(전체 서비스)"
			}
			bucketTime, _ := time.Parse(time.RFC3339, p.key.Bucket)
			fmt.Printf("%-18s %-20s %s  %d\n",
				p.key.ServiceName, opName, bucketTime.Format("2006-01-02 15:00"), p.count)
		}
	}
	fmt.Printf("\n총 파티션 수: %d\n", len(partitions))
	fmt.Println()
	fmt.Println("설계 의도:")
	fmt.Println("  - 1시간 버킷으로 파티션 크기를 제한 → 핫 파티션 방지")
	fmt.Println("  - operation_name=\"\"인 엔트리 → 서비스 전체 검색 시 사용")
	fmt.Println("  - operation_name=실제값인 엔트리 → 특정 오퍼레이션 검색 시 사용")

	// =========================================================================
	// 3단계: Duration 범위 쿼리 시뮬레이션
	// =========================================================================
	printSeparator("3단계: Duration 범위 쿼리 시뮬레이션")

	printSubSeparator("쿼리 A: api-gateway 서비스, 100ms~500ms 범위")
	queryA := TraceQueryParameters{
		ServiceName:  "api-gateway",
		StartTimeMin: baseTime,
		StartTimeMax: baseTime.Add(6 * time.Hour),
		DurationMin:  100 * time.Millisecond,
		DurationMax:  500 * time.Millisecond,
		NumTraces:    10,
	}
	resultA := store.QueryByDuration(queryA)
	fmt.Printf("검색 결과: %d개 트레이스\n", len(resultA.TraceIDs))
	fmt.Printf("스캔한 버킷 수: %d, 스캔한 엔트리 수: %d\n", resultA.BucketsScanned, resultA.EntriesScanned)
	for i, id := range resultA.TraceIDs {
		if i >= 5 {
			fmt.Printf("  ... 외 %d개\n", len(resultA.TraceIDs)-5)
			break
		}
		fmt.Printf("  [%d] %s\n", i+1, id)
	}

	printSubSeparator("쿼리 B: user-service/GetUser, 1s 이상 느린 요청")
	queryB := TraceQueryParameters{
		ServiceName:   "user-service",
		OperationName: "GetUser",
		StartTimeMin:  baseTime,
		StartTimeMax:  baseTime.Add(6 * time.Hour),
		DurationMin:   1 * time.Second,
		DurationMax:   10 * time.Second,
		NumTraces:     10,
	}
	resultB := store.QueryByDuration(queryB)
	fmt.Printf("검색 결과: %d개 트레이스\n", len(resultB.TraceIDs))
	fmt.Printf("스캔한 버킷 수: %d, 스캔한 엔트리 수: %d\n", resultB.BucketsScanned, resultB.EntriesScanned)
	for i, id := range resultB.TraceIDs {
		if i >= 5 {
			fmt.Printf("  ... 외 %d개\n", len(resultB.TraceIDs)-5)
			break
		}
		fmt.Printf("  [%d] %s\n", i+1, id)
	}

	printSubSeparator("쿼리 C: 좁은 시간 범위 (1시간) → 1개 버킷만 스캔")
	queryC := TraceQueryParameters{
		ServiceName:  "order-service",
		StartTimeMin: baseTime.Add(2 * time.Hour),
		StartTimeMax: baseTime.Add(3 * time.Hour),
		DurationMin:  50 * time.Millisecond,
		DurationMax:  2 * time.Second,
		NumTraces:    10,
	}
	resultC := store.QueryByDuration(queryC)
	fmt.Printf("검색 결과: %d개 트레이스\n", len(resultC.TraceIDs))
	fmt.Printf("스캔한 버킷 수: %d (좁은 시간 범위 → 효율적 쿼리)\n", resultC.BucketsScanned)

	// =========================================================================
	// 4단계: Duration과 태그 교차 쿼리 불가 시연
	// =========================================================================
	printSeparator("4단계: Duration + 태그 교차 쿼리 불가 시연")

	fmt.Println()
	fmt.Println("Jaeger Cassandra에서 duration과 태그를 동시에 쿼리할 수 없는 이유:")
	fmt.Println()
	fmt.Println("  duration_index 파티션 키: (service_name, operation_name, bucket)")
	fmt.Println("  tag_index 파티션 키:      (service_name, tag_key, tag_value)")
	fmt.Println()
	fmt.Println("  → Cassandra는 서버 사이드 조인을 지원하지 않음")
	fmt.Println("  → 두 인덱스의 파티션 키가 완전히 다름")
	fmt.Println("  → 클라이언트 사이드 교차는 대량 데이터에서 비효율적")
	fmt.Println()

	// 태그+duration 동시 쿼리 시도
	invalidQuery := TraceQueryParameters{
		ServiceName:  "api-gateway",
		StartTimeMin: baseTime,
		StartTimeMax: baseTime.Add(6 * time.Hour),
		DurationMin:  100 * time.Millisecond,
		DurationMax:  500 * time.Millisecond,
		Tags:         map[string]string{"http.status_code": "500"},
	}

	if err := ValidateQuery(invalidQuery); err != nil {
		fmt.Printf("유효성 검사 실패: %s\n", err)
	}

	// 실제로 해야 하는 방법: 별도 쿼리 후 클라이언트에서 교차
	printSubSeparator("우회 방법: 별도 쿼리 후 클라이언트 사이드 교차")

	durationQuery := TraceQueryParameters{
		ServiceName:  "api-gateway",
		StartTimeMin: baseTime,
		StartTimeMax: baseTime.Add(6 * time.Hour),
		DurationMin:  100 * time.Millisecond,
		DurationMax:  2000 * time.Millisecond,
		NumTraces:    100,
	}
	durationResult := store.QueryByDuration(durationQuery)

	tagResult := store.QueryByTag("api-gateway", "http.status_code", "500",
		baseTime, baseTime.Add(6*time.Hour))

	// 클라이언트 사이드 교차
	durationSet := make(map[TraceID]bool)
	for _, id := range durationResult.TraceIDs {
		durationSet[id] = true
	}
	var intersection []TraceID
	for _, id := range tagResult {
		if durationSet[id] {
			intersection = append(intersection, id)
		}
	}

	fmt.Printf("Duration 쿼리 결과: %d개 트레이스\n", len(durationResult.TraceIDs))
	fmt.Printf("태그 쿼리 결과 (status=500): %d개 트레이스\n", len(tagResult))
	fmt.Printf("클라이언트 사이드 교차 결과: %d개 트레이스\n", len(intersection))

	// =========================================================================
	// 5단계: 두 개 엔트리 전략의 효과 시연
	// =========================================================================
	printSeparator("5단계: 스팬당 2개 인덱스 엔트리 전략 효과")

	fmt.Println()
	fmt.Println("indexByDuration은 스팬 하나당 2개의 인덱스 엔트리를 생성합니다:")
	fmt.Println("  indexByOperationName(\"\")              → 서비스 전체 검색")
	fmt.Println("  indexByOperationName(span.OperationName) → 특정 오퍼레이션 검색")
	fmt.Println()

	// 서비스 전체 검색 (operation_name="")
	queryAll := TraceQueryParameters{
		ServiceName:   "user-service",
		OperationName: "", // 빈 문자열 → operation_name="" 파티션 검색
		StartTimeMin:  baseTime,
		StartTimeMax:  baseTime.Add(6 * time.Hour),
		DurationMin:   500 * time.Millisecond,
		DurationMax:   5 * time.Second,
		NumTraces:     20,
	}
	resultAll := store.QueryByDuration(queryAll)

	// 특정 오퍼레이션 검색
	querySpecific := TraceQueryParameters{
		ServiceName:   "user-service",
		OperationName: "GetUser",
		StartTimeMin:  baseTime,
		StartTimeMax:  baseTime.Add(6 * time.Hour),
		DurationMin:   500 * time.Millisecond,
		DurationMax:   5 * time.Second,
		NumTraces:     20,
	}
	resultSpecific := store.QueryByDuration(querySpecific)

	fmt.Printf("user-service 전체 (operation=\"\"): %d개 트레이스 발견\n", len(resultAll.TraceIDs))
	fmt.Printf("user-service/GetUser만:           %d개 트레이스 발견\n", len(resultSpecific.TraceIDs))
	fmt.Println()
	fmt.Println("→ operation_name=\"\"인 엔트리는 서비스의 모든 오퍼레이션을 포함합니다.")
	fmt.Println("→ 사용자가 오퍼레이션을 지정하지 않으면 \"\" 파티션을 검색합니다.")
	fmt.Println("→ 이 전략으로 별도의 서비스 전용 인덱스 없이도 서비스 전체 검색이 가능합니다.")

	// =========================================================================
	// 6단계: 쿼리 분포 및 성능 분석
	// =========================================================================
	printSeparator("6단계: 버킷 전략의 쿼리 분포 효과")

	fmt.Println()
	fmt.Println("시간 범위에 따른 스캔 버킷 수 비교:")
	fmt.Println()

	timeRanges := []struct {
		name     string
		duration time.Duration
	}{
		{"1시간", 1 * time.Hour},
		{"3시간", 3 * time.Hour},
		{"6시간", 6 * time.Hour},
		{"12시간", 12 * time.Hour},
		{"24시간", 24 * time.Hour},
	}

	fmt.Printf("%-10s  스캔 버킷 수  Cassandra 쿼리 수\n", "시간 범위")
	fmt.Println(strings.Repeat("-", 50))
	for _, tr := range timeRanges {
		q := TraceQueryParameters{
			ServiceName:  "api-gateway",
			StartTimeMin: baseTime,
			StartTimeMax: baseTime.Add(tr.duration),
			DurationMin:  100 * time.Millisecond,
			NumTraces:    10,
		}
		r := store.QueryByDuration(q)
		fmt.Printf("%-10s  %d개          %d회\n", tr.name, r.BucketsScanned, r.BucketsScanned)
	}

	fmt.Println()
	fmt.Println("핵심 인사이트:")
	fmt.Println("  1. 시간 범위가 넓을수록 더 많은 버킷(파티션)을 스캔해야 함")
	fmt.Println("  2. 각 버킷은 독립된 Cassandra 쿼리 → 버킷 수 = 쿼리 수")
	fmt.Println("  3. 쿼리는 endTime부터 역순으로 진행 → 최신 데이터 우선 반환")
	fmt.Println("  4. NumTraces에 도달하면 조기 종료 → 불필요한 버킷 스캔 방지")
	fmt.Println("  5. 1시간 버킷 크기는 파티션 크기 제한과 쿼리 효율의 균형점")

	// =========================================================================
	// CQL 스키마 참조
	// =========================================================================
	printSeparator("참고: 실제 Cassandra CQL 스키마")

	fmt.Println()
	fmt.Println("-- duration_index 테이블 (writer.go L47-L50)")
	fmt.Println("INSERT INTO duration_index(")
	fmt.Println("    service_name,     -- 파티션 키 1")
	fmt.Println("    operation_name,   -- 파티션 키 2")
	fmt.Println("    bucket,           -- 파티션 키 3 (1시간 단위)")
	fmt.Println("    duration,         -- 클러스터링 컬럼 (범위 필터링)")
	fmt.Println("    start_time,")
	fmt.Println("    trace_id")
	fmt.Println(") VALUES (?, ?, ?, ?, ?, ?)")
	fmt.Println()
	fmt.Println("-- duration_index 쿼리 (reader.go L52-L56)")
	fmt.Println("SELECT trace_id")
	fmt.Println("FROM duration_index")
	fmt.Println("WHERE bucket = ?")
	fmt.Println("  AND service_name = ?")
	fmt.Println("  AND operation_name = ?")
	fmt.Println("  AND duration > ?     -- 클러스터링 컬럼 범위 필터")
	fmt.Println("  AND duration < ?")
	fmt.Println("LIMIT ?")
	fmt.Println()
	fmt.Println("핵심: 파티션 키(bucket, service_name, operation_name)는 등호(=) 조건만 가능")
	fmt.Println("      클러스터링 컬럼(duration)은 범위(>, <) 조건 가능")
	fmt.Println("      → 이것이 duration 쿼리와 tag 쿼리를 교차할 수 없는 근본 원인")
}
