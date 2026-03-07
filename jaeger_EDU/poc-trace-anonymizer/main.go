package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Jaeger 트레이스 익명화(Anonymizer) 시뮬레이션
//
// 실제 소스코드 참조:
//   - cmd/anonymizer/app/anonymizer/anonymizer.go: Anonymizer, hash(), allowedTags
//   - cmd/anonymizer/app/writer/writer.go: Writer, WriteSpan
//
// 핵심 설계:
//   1. FNV64 해싱으로 서비스명/오퍼레이션명 비가역 변환
//   2. allowedTags: error, http.method, http.status_code, span.kind 등은 보존
//   3. Options로 커스텀 태그, 로그, 프로세스 속성 해싱 제어
//   4. 매핑 파일로 원본↔해시 대응 관계 유지 (역추적용)
//   5. 같은 입력은 항상 같은 해시값 (결정적 해싱)
// =============================================================================

// ---------------------------------------------------------------------------
// 허용된 표준 태그 (절대 해싱되지 않음)
// 실제 코드: anonymizer.go L23-L30
// ---------------------------------------------------------------------------

var allowedTags = map[string]bool{
	"error":            true,
	"http.method":      true,
	"http.status_code": true,
	"span.kind":        true,
	"sampler.type":     true,  // model.SamplerTypeKey
	"sampler.param":    true,  // model.SamplerParamKey
}

// ---------------------------------------------------------------------------
// 데이터 모델
// ---------------------------------------------------------------------------

// KeyValue는 태그/로그 필드의 키-값 쌍
type KeyValue struct {
	Key   string `json:"key"`
	Type  string `json:"type"`  // string, bool, int64, float64
	Value string `json:"value"` // 문자열로 통일
}

// Log는 스팬의 로그 이벤트
type Log struct {
	Timestamp time.Time  `json:"timestamp"`
	Fields    []KeyValue `json:"fields"`
}

// Process는 스팬을 생성한 프로세스 정보
type Process struct {
	ServiceName string     `json:"serviceName"`
	Tags        []KeyValue `json:"tags,omitempty"`
}

// Span은 하나의 트레이싱 단위
type Span struct {
	TraceID       string     `json:"traceID"`
	SpanID        string     `json:"spanID"`
	OperationName string     `json:"operationName"`
	Process       Process    `json:"process"`
	Tags          []KeyValue `json:"tags,omitempty"`
	Logs          []Log      `json:"logs,omitempty"`
	StartTime     int64      `json:"startTime"`
	Duration      int64      `json:"duration"`
}

// ---------------------------------------------------------------------------
// 매핑 저장소
// 실제 코드: anonymizer.go L36-L39
// ---------------------------------------------------------------------------

// Mapping은 원본 → 해시 대응 관계를 유지
type Mapping struct {
	Services   map[string]string `json:"services"`
	Operations map[string]string `json:"operations"`
}

// ---------------------------------------------------------------------------
// 익명화 옵션
// 실제 코드: anonymizer.go L57-L62
// ---------------------------------------------------------------------------

// Options는 익명화 동작을 제어하는 옵션
type Options struct {
	HashStandardTags bool `json:"hash_standard_tags"` // 표준 태그도 해싱할지
	HashCustomTags   bool `json:"hash_custom_tags"`   // 비표준 태그를 해싱할지 (false면 삭제)
	HashLogs         bool `json:"hash_logs"`          // 로그를 해싱할지 (false면 삭제)
	HashProcess      bool `json:"hash_process"`       // 프로세스 속성을 해싱할지 (false면 삭제)
}

// ---------------------------------------------------------------------------
// Anonymizer: 핵심 익명화 엔진
// 실제 코드: anonymizer.go L46-L54
// ---------------------------------------------------------------------------

// Anonymizer는 Jaeger 스팬을 익명화하는 엔진
type Anonymizer struct {
	mappingFile string
	lock        sync.Mutex
	mapping     Mapping
	options     Options
}

// New는 새 Anonymizer를 생성
// 실제 코드: anonymizer.go L66-L99
func New(mappingFile string, options Options) *Anonymizer {
	a := &Anonymizer{
		mappingFile: mappingFile,
		mapping: Mapping{
			Services:   make(map[string]string),
			Operations: make(map[string]string),
		},
		options: options,
	}

	// 기존 매핑 파일이 있으면 로드 (실제 코드: L78-L86)
	if data, err := os.ReadFile(mappingFile); err == nil {
		if err := json.Unmarshal(data, &a.mapping); err != nil {
			fmt.Printf("매핑 파일 파싱 실패: %v\n", err)
		} else {
			fmt.Printf("기존 매핑 파일 로드: 서비스 %d개, 오퍼레이션 %d개\n",
				len(a.mapping.Services), len(a.mapping.Operations))
		}
	}

	return a
}

// hash는 FNV64 해시를 16진수 문자열로 반환
// 실제 코드: anonymizer.go L145-L149
func hash(value string) string {
	h := fnv.New64()
	h.Write([]byte(value))
	return fmt.Sprintf("%016x", h.Sum64())
}

// mapServiceName은 서비스명을 해싱하고 매핑에 기록
// 실제 코드: anonymizer.go L125-L127
func (a *Anonymizer) mapServiceName(service string) string {
	return a.mapString(service, a.mapping.Services)
}

// mapOperationName은 [서비스]:오퍼레이션 형식으로 키를 만들어 해싱
// 실제 코드: anonymizer.go L129-L132
func (a *Anonymizer) mapOperationName(service, operation string) string {
	v := fmt.Sprintf("[%s]:%s", service, operation)
	return a.mapString(v, a.mapping.Operations)
}

// mapString은 범용 문자열 해싱 + 매핑 저장
// 실제 코드: anonymizer.go L134-L143
func (a *Anonymizer) mapString(v string, m map[string]string) string {
	a.lock.Lock()
	defer a.lock.Unlock()
	if s, ok := m[v]; ok {
		return s // 이미 해싱된 값 반환 (일관성 보장)
	}
	s := hash(v)
	m[v] = s
	return s
}

// filterStandardTags는 allowedTags에 포함된 태그만 반환
// 실제 코드: anonymizer.go L191-L212
func filterStandardTags(tags []KeyValue) []KeyValue {
	out := make([]KeyValue, 0, len(tags))
	for _, tag := range tags {
		if !allowedTags[tag.Key] {
			continue
		}
		// error 태그는 bool 값으로 정규화 (실제 코드: L197-L207)
		if tag.Key == "error" {
			if tag.Type == "bool" {
				// 그대로 유지
			} else if tag.Type == "string" && (tag.Value == "true" || tag.Value == "false") {
				// 그대로 유지
			} else {
				tag = KeyValue{Key: "error", Type: "bool", Value: "true"}
			}
		}
		out = append(out, tag)
	}
	return out
}

// filterCustomTags는 allowedTags에 포함되지 않은 태그만 반환
// 실제 코드: anonymizer.go L215-L223
func filterCustomTags(tags []KeyValue) []KeyValue {
	out := make([]KeyValue, 0, len(tags))
	for _, tag := range tags {
		if !allowedTags[tag.Key] {
			out = append(out, tag)
		}
	}
	return out
}

// hashTags는 태그의 키와 값을 모두 해싱
// 실제 코드: anonymizer.go L227-L234
func hashTags(tags []KeyValue) []KeyValue {
	out := make([]KeyValue, 0, len(tags))
	for _, tag := range tags {
		kv := KeyValue{
			Key:   hash(tag.Key),
			Type:  "string",
			Value: hash(tag.Value),
		}
		out = append(out, kv)
	}
	return out
}

// AnonymizeSpan은 스팬을 익명화
// 실제 코드: anonymizer.go L152-L188
func (a *Anonymizer) AnonymizeSpan(span Span) Span {
	service := span.Process.ServiceName

	// 오퍼레이션명 해싱 (원본 서비스명 기준)
	span.OperationName = a.mapOperationName(service, span.OperationName)

	// 표준 태그 필터링
	outputTags := filterStandardTags(span.Tags)

	// HashStandardTags: true이면 표준 태그도 해싱 (실제 코드: L158-L160)
	if a.options.HashStandardTags {
		outputTags = hashTags(outputTags)
	}

	// HashCustomTags: true이면 비표준 태그를 해싱하여 추가, false이면 삭제 (실제 코드: L162-L165)
	if a.options.HashCustomTags {
		customTags := hashTags(filterCustomTags(span.Tags))
		outputTags = append(outputTags, customTags...)
	}
	span.Tags = outputTags

	// HashLogs: true이면 로그 필드 해싱, false이면 삭제 (실제 코드: L168-L175)
	if a.options.HashLogs {
		for i := range span.Logs {
			span.Logs[i].Fields = hashTags(span.Logs[i].Fields)
		}
	} else {
		span.Logs = nil
	}

	// 서비스명 해싱 (실제 코드: L177)
	span.Process.ServiceName = a.mapServiceName(service)

	// HashProcess: true이면 프로세스 태그 해싱, false이면 삭제 (실제 코드: L179-L184)
	if a.options.HashProcess {
		span.Process.Tags = hashTags(span.Process.Tags)
	} else {
		span.Process.Tags = nil
	}

	return span
}

// SaveMapping은 매핑 파일을 저장
// 실제 코드: anonymizer.go L110-L123
func (a *Anonymizer) SaveMapping() error {
	a.lock.Lock()
	defer a.lock.Unlock()
	data, err := json.MarshalIndent(a.mapping, "", "  ")
	if err != nil {
		return fmt.Errorf("매핑 직렬화 실패: %w", err)
	}
	if err := os.WriteFile(a.mappingFile, data, 0600); err != nil {
		return fmt.Errorf("매핑 파일 쓰기 실패: %w", err)
	}
	return nil
}

// GetMapping은 현재 매핑을 반환 (읽기 전용)
func (a *Anonymizer) GetMapping() Mapping {
	a.lock.Lock()
	defer a.lock.Unlock()
	return a.mapping
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

func printSpanJSON(label string, span Span) {
	data, _ := json.MarshalIndent(span, "  ", "  ")
	fmt.Printf("%s:\n  %s\n", label, string(data))
}

// ---------------------------------------------------------------------------
// 메인: 시뮬레이션 실행
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("Jaeger 트레이스 익명화(Anonymizer) 시뮬레이션")
	fmt.Println("참조: cmd/anonymizer/app/anonymizer/anonymizer.go")
	fmt.Println("참조: cmd/anonymizer/app/writer/writer.go")

	// 임시 매핑 파일
	mappingFile := "/tmp/jaeger_poc_anonymizer_mapping.json"

	// =========================================================================
	// 1단계: FNV64 해싱 기본 동작
	// =========================================================================
	printSeparator("1단계: FNV64 해싱 기본 동작")

	fmt.Println()
	fmt.Println("Jaeger는 FNV-1a 64비트 해시를 사용하여 서비스/오퍼레이션명을 비가역 변환합니다.")
	fmt.Println("실제 코드: anonymizer.go L145-L149")
	fmt.Println()
	fmt.Println("  func hash(value string) string {")
	fmt.Println("      h := fnv.New64()")
	fmt.Println("      h.Write([]byte(value))")
	fmt.Println("      return fmt.Sprintf(\"%016x\", h.Sum64())")
	fmt.Println("  }")
	fmt.Println()

	testValues := []string{
		"payment-service",
		"user-api",
		"ProcessPayment",
		"GET /api/v1/users",
		"payment-service", // 중복: 같은 입력은 같은 출력
	}

	fmt.Printf("%-30s → %-20s\n", "원본 값", "FNV64 해시")
	fmt.Println(strings.Repeat("-", 55))
	for _, v := range testValues {
		fmt.Printf("%-30s → %s\n", v, hash(v))
	}
	fmt.Println()
	fmt.Println("핵심: 같은 입력은 항상 같은 해시값 (결정적 해싱)")
	fmt.Println("      16바이트 16진수 문자열 = 64비트 해시")

	// =========================================================================
	// 2단계: 허용된 표준 태그
	// =========================================================================
	printSeparator("2단계: 허용된 표준 태그 (allowedTags)")

	fmt.Println()
	fmt.Println("아래 태그는 익명화 시에도 보존됩니다 (해싱되지 않음):")
	fmt.Println("실제 코드: anonymizer.go L23-L30")
	fmt.Println()

	sortedTags := make([]string, 0, len(allowedTags))
	for tag := range allowedTags {
		sortedTags = append(sortedTags, tag)
	}
	sort.Strings(sortedTags)
	for _, tag := range sortedTags {
		fmt.Printf("  - %s\n", tag)
	}

	fmt.Println()
	fmt.Println("이유: 이 태그들은 트레이스 분석에 필수적이며, 사이트 특정 정보를 포함하지 않습니다.")

	// =========================================================================
	// 3단계: 기본 익명화 (커스텀 태그 삭제)
	// =========================================================================
	printSeparator("3단계: 기본 익명화 (Options 기본값)")

	// 원본 스팬 생성
	originalSpan := Span{
		TraceID:       "abc123def456",
		SpanID:        "span001",
		OperationName: "ProcessPayment",
		Process: Process{
			ServiceName: "payment-service",
			Tags: []KeyValue{
				{Key: "hostname", Type: "string", Value: "prod-payment-01.internal.corp"},
				{Key: "ip", Type: "string", Value: "10.0.5.42"},
				{Key: "jaeger.version", Type: "string", Value: "1.35.0"},
			},
		},
		Tags: []KeyValue{
			{Key: "http.method", Type: "string", Value: "POST"},
			{Key: "http.status_code", Type: "string", Value: "200"},
			{Key: "span.kind", Type: "string", Value: "server"},
			{Key: "error", Type: "bool", Value: "false"},
			{Key: "db.statement", Type: "string", Value: "INSERT INTO payments (id, amount) VALUES (12345, 99.99)"},
			{Key: "customer.id", Type: "string", Value: "user-42"},
			{Key: "payment.method", Type: "string", Value: "credit-card"},
		},
		Logs: []Log{
			{
				Timestamp: time.Now(),
				Fields: []KeyValue{
					{Key: "event", Type: "string", Value: "payment_processed"},
					{Key: "message", Type: "string", Value: "Payment of $99.99 processed for customer user-42"},
				},
			},
		},
		StartTime: time.Now().UnixMicro(),
		Duration:  150000, // 150ms in microseconds
	}

	printSubSeparator("원본 스팬")
	printSpanJSON("원본", originalSpan)

	// 기본 옵션으로 익명화 (모든 해싱 비활성화)
	anonymizer := New(mappingFile, Options{
		HashStandardTags: false,
		HashCustomTags:   false,
		HashLogs:         false,
		HashProcess:      false,
	})

	anonymized := anonymizer.AnonymizeSpan(originalSpan)

	printSubSeparator("익명화 결과 (기본 옵션: 커스텀 태그/로그/프로세스 태그 삭제)")
	printSpanJSON("익명화", anonymized)

	fmt.Println()
	fmt.Println("관찰 포인트:")
	fmt.Println("  1. serviceName이 해시값으로 변환됨")
	fmt.Println("  2. operationName이 해시값으로 변환됨")
	fmt.Println("  3. 표준 태그(http.method, http.status_code, span.kind, error)는 보존")
	fmt.Println("  4. 커스텀 태그(db.statement, customer.id, payment.method)는 삭제됨")
	fmt.Println("  5. 로그는 삭제됨")
	fmt.Println("  6. 프로세스 태그(hostname, ip)는 삭제됨")
	fmt.Println("  7. traceID, spanID, startTime, duration은 보존")

	// =========================================================================
	// 4단계: 모든 해싱 옵션 활성화
	// =========================================================================
	printSeparator("4단계: 모든 해싱 옵션 활성화")

	anonymizerFull := New(mappingFile+"_full", Options{
		HashStandardTags: true,
		HashCustomTags:   true,
		HashLogs:         true,
		HashProcess:      true,
	})

	// 원본 스팬을 깊은 복사
	fullSpan := Span{
		TraceID:       originalSpan.TraceID,
		SpanID:        originalSpan.SpanID,
		OperationName: originalSpan.OperationName,
		Process: Process{
			ServiceName: originalSpan.Process.ServiceName,
			Tags:        append([]KeyValue{}, originalSpan.Process.Tags...),
		},
		Tags:      append([]KeyValue{}, originalSpan.Tags...),
		Logs:      make([]Log, len(originalSpan.Logs)),
		StartTime: originalSpan.StartTime,
		Duration:  originalSpan.Duration,
	}
	for i, log := range originalSpan.Logs {
		fullSpan.Logs[i] = Log{
			Timestamp: log.Timestamp,
			Fields:    append([]KeyValue{}, log.Fields...),
		}
	}

	anonymizedFull := anonymizerFull.AnonymizeSpan(fullSpan)

	printSubSeparator("익명화 결과 (전체 해싱 활성화)")
	printSpanJSON("전체 해싱", anonymizedFull)

	fmt.Println()
	fmt.Println("관찰 포인트:")
	fmt.Println("  1. 표준 태그도 키와 값이 모두 해싱됨 (HashStandardTags=true)")
	fmt.Println("  2. 커스텀 태그가 삭제 대신 해싱됨 (HashCustomTags=true)")
	fmt.Println("  3. 로그 필드가 삭제 대신 해싱됨 (HashLogs=true)")
	fmt.Println("  4. 프로세스 태그가 삭제 대신 해싱됨 (HashProcess=true)")

	// =========================================================================
	// 5단계: 매핑 파일 확인
	// =========================================================================
	printSeparator("5단계: 매핑 파일 (역추적용)")

	fmt.Println()
	fmt.Println("매핑 파일은 원본 값 → 해시 값의 대응 관계를 저장합니다.")
	fmt.Println("연구자가 질문이 있을 때 원본 값을 역추적할 수 있도록 합니다.")
	fmt.Println("실제 코드: anonymizer.go L36-L39")
	fmt.Println()

	mapping := anonymizer.GetMapping()

	printSubSeparator("서비스 매핑")
	for original, hashed := range mapping.Services {
		fmt.Printf("  %-30s → %s\n", original, hashed)
	}

	printSubSeparator("오퍼레이션 매핑")
	for original, hashed := range mapping.Operations {
		fmt.Printf("  %-45s → %s\n", original, hashed)
	}

	// 매핑 파일 저장
	if err := anonymizer.SaveMapping(); err != nil {
		fmt.Printf("매핑 파일 저장 실패: %v\n", err)
	} else {
		fmt.Printf("\n매핑 파일 저장 완료: %s\n", mappingFile)
	}

	// =========================================================================
	// 6단계: 일관성 검증 (같은 입력 → 같은 해시)
	// =========================================================================
	printSeparator("6단계: 일관성 검증 (결정적 해싱)")

	fmt.Println()
	fmt.Println("같은 서비스/오퍼레이션은 항상 같은 해시값을 반환해야 합니다.")
	fmt.Println("이를 통해 여러 스팬의 관계를 익명화 후에도 유지할 수 있습니다.")
	fmt.Println()

	// 같은 서비스의 다른 스팬
	span2 := Span{
		TraceID:       "xyz789",
		SpanID:        "span002",
		OperationName: "ProcessPayment", // 같은 오퍼레이션
		Process: Process{
			ServiceName: "payment-service", // 같은 서비스
		},
		Tags: []KeyValue{
			{Key: "http.method", Type: "string", Value: "POST"},
		},
		StartTime: time.Now().UnixMicro(),
		Duration:  200000,
	}

	anonymized2 := anonymizer.AnonymizeSpan(span2)

	fmt.Printf("스팬1 서비스:    %s\n", anonymized.Process.ServiceName)
	fmt.Printf("스팬2 서비스:    %s\n", anonymized2.Process.ServiceName)
	fmt.Printf("서비스 일치:     %v\n", anonymized.Process.ServiceName == anonymized2.Process.ServiceName)
	fmt.Println()
	fmt.Printf("스팬1 오퍼레이션: %s\n", anonymized.OperationName)
	fmt.Printf("스팬2 오퍼레이션: %s\n", anonymized2.OperationName)
	fmt.Printf("오퍼레이션 일치:  %v\n", anonymized.OperationName == anonymized2.OperationName)
	fmt.Println()
	fmt.Println("핵심: 같은 서비스+오퍼레이션 조합은 항상 동일한 해시 → 익명화 후에도 트레이스 분석 가능")

	// =========================================================================
	// 7단계: 매핑 파일 재사용 (런 간 일관성)
	// =========================================================================
	printSeparator("7단계: 매핑 파일 재사용 (런 간 일관성)")

	fmt.Println()
	fmt.Println("매핑 파일을 재사용하면 서로 다른 실행에서도 같은 해시값을 보장합니다.")
	fmt.Println("실제 코드: anonymizer.go L78-L86 (기존 매핑 로드)")
	fmt.Println()

	// 새 Anonymizer가 기존 매핑 파일을 로드
	anonymizer2 := New(mappingFile, Options{
		HashStandardTags: false,
		HashCustomTags:   false,
		HashLogs:         false,
		HashProcess:      false,
	})

	span3 := Span{
		TraceID:       "new-trace-999",
		SpanID:        "span999",
		OperationName: "ProcessPayment",
		Process: Process{
			ServiceName: "payment-service",
		},
		Tags: []KeyValue{
			{Key: "http.method", Type: "string", Value: "POST"},
		},
	}

	anonymized3 := anonymizer2.AnonymizeSpan(span3)

	fmt.Printf("원래 Anonymizer 결과: service=%s\n", anonymized.Process.ServiceName)
	fmt.Printf("새 Anonymizer 결과:   service=%s\n", anonymized3.Process.ServiceName)
	fmt.Printf("런 간 일치:           %v\n", anonymized.Process.ServiceName == anonymized3.Process.ServiceName)

	// =========================================================================
	// 8단계: error 태그 정규화
	// =========================================================================
	printSeparator("8단계: error 태그 정규화")

	fmt.Println()
	fmt.Println("error 태그는 특별 처리됩니다:")
	fmt.Println("  - bool 타입: 그대로 유지")
	fmt.Println("  - string 타입, 값이 'true'/'false': 그대로 유지")
	fmt.Println("  - 그 외: Bool(error, true)로 강제 변환")
	fmt.Println("실제 코드: anonymizer.go L197-L207")
	fmt.Println()

	errorTestCases := []struct {
		name string
		tag  KeyValue
	}{
		{"bool false", KeyValue{Key: "error", Type: "bool", Value: "false"}},
		{"bool true", KeyValue{Key: "error", Type: "bool", Value: "true"}},
		{"string 'true'", KeyValue{Key: "error", Type: "string", Value: "true"}},
		{"string 'Connection refused'", KeyValue{Key: "error", Type: "string", Value: "Connection refused"}},
		{"int64 '1'", KeyValue{Key: "error", Type: "int64", Value: "1"}},
	}

	fmt.Printf("%-30s → %-10s %-10s\n", "입력", "타입", "값")
	fmt.Println(strings.Repeat("-", 55))
	for _, tc := range errorTestCases {
		filtered := filterStandardTags([]KeyValue{tc.tag})
		if len(filtered) > 0 {
			fmt.Printf("%-30s → %-10s %-10s\n", tc.name, filtered[0].Type, filtered[0].Value)
		}
	}

	// =========================================================================
	// 요약 다이어그램
	// =========================================================================
	printSeparator("요약: 익명화 파이프라인")

	fmt.Println(`
    원본 스팬 (민감한 데이터 포함)
    +------------------------------------------+
    | serviceName: "payment-service"            |
    | operationName: "ProcessPayment"           |
    | tags:                                     |
    |   http.method: "POST"      (표준 태그)    |
    |   customer.id: "user-42"   (커스텀 태그)  |
    |   db.statement: "INSERT..." (커스텀 태그) |
    | logs:                                     |
    |   message: "Payment of $99.99..."         |
    | process.tags:                             |
    |   hostname: "prod-payment-01..."          |
    +------------------------------------------+
                      |
                      v
    +------------------------------------------+
    |         Anonymizer.AnonymizeSpan()        |
    |                                           |
    | 1. operationName → FNV64 해시             |
    | 2. 표준 태그 필터링 (allowedTags)         |
    |    - HashStandardTags? → 해시/보존        |
    | 3. 커스텀 태그 처리                       |
    |    - HashCustomTags? → 해시/삭제          |
    | 4. 로그 처리                              |
    |    - HashLogs? → 해시/삭제                |
    | 5. serviceName → FNV64 해시               |
    | 6. 프로세스 태그 처리                     |
    |    - HashProcess? → 해시/삭제             |
    +------------------------------------------+
                      |
                      v
    익명화된 스팬 (안전하게 공유 가능)
    +------------------------------------------+
    | serviceName: "a1b2c3d4e5f67890"           |
    | operationName: "1234567890abcdef"         |
    | tags:                                     |
    |   http.method: "POST"      (보존됨)       |
    |   (커스텀 태그 삭제 또는 해시됨)          |
    | logs: null  (삭제 또는 해시됨)            |
    | process.tags: null (삭제 또는 해시됨)     |
    +------------------------------------------+
                      +
                      |
    매핑 파일 (mapping.json)
    +------------------------------------------+
    | services:                                 |
    |   "payment-service" → "a1b2c3d4e5f67890"  |
    | operations:                               |
    |   "[payment-service]:ProcessPayment"      |
    |     → "1234567890abcdef"                  |
    +------------------------------------------+
`)

	// 임시 파일 정리
	os.Remove(mappingFile)
	os.Remove(mappingFile + "_full")
}
