package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// =============================================================================
// Kafka Connect Transforms + File Connector 시뮬레이션
//
// 이 PoC는 Kafka Connect의 핵심 개념을 시뮬레이션한다:
//   1. SMT (Single Message Transform) 체인
//   2. 내장 Transform: InsertField, ReplaceField, MaskField, TimestampRouter 등
//   3. FileStreamSource/Sink Connector
//   4. Converter (JSON/String) 시뮬레이션
//   5. Dead Letter Queue 처리
//
// 참조 소스:
//   connect/transforms/src/main/java/.../InsertField.java
//   connect/transforms/src/main/java/.../ReplaceField.java
//   connect/transforms/src/main/java/.../TimestampRouter.java
//   connect/file/src/main/java/.../FileStreamSourceConnector.java
//   connect/file/src/main/java/.../FileStreamSinkConnector.java
// =============================================================================

// --- ConnectRecord: Connect 레코드 ---

type ConnectRecord struct {
	Topic     string
	Key       interface{}
	Value     map[string]interface{}
	Headers   map[string]string
	Timestamp time.Time
	Offset    int64
}

func (r ConnectRecord) String() string {
	return fmt.Sprintf("{topic=%s, key=%v, value=%v, headers=%v}",
		r.Topic, r.Key, r.Value, r.Headers)
}

// --- Transform 인터페이스 ---
// 실제: connect/api/src/main/java/org/apache/kafka/connect/transforms/Transformation.java

type Transform interface {
	Apply(record ConnectRecord) (*ConnectRecord, error)
	Name() string
}

// --- InsertField: 필드 추가 ---
// 실제: connect/transforms/src/main/java/.../InsertField.java

type InsertField struct {
	StaticField string
	StaticValue interface{}
	TimestampField string // 빈 문자열이면 추가 안 함
}

func (t *InsertField) Apply(record ConnectRecord) (*ConnectRecord, error) {
	if record.Value == nil {
		record.Value = make(map[string]interface{})
	}
	if t.StaticField != "" {
		record.Value[t.StaticField] = t.StaticValue
	}
	if t.TimestampField != "" {
		record.Value[t.TimestampField] = record.Timestamp.Format(time.RFC3339)
	}
	return &record, nil
}

func (t *InsertField) Name() string { return "InsertField" }

// --- ReplaceField: 필드 이름 변경/제거/포함 ---
// 실제: connect/transforms/src/main/java/.../ReplaceField.java

type ReplaceField struct {
	Renames  map[string]string // oldName → newName
	Excludes []string          // 제거할 필드
	Includes []string          // 포함할 필드 (비어있으면 모두 포함)
}

func (t *ReplaceField) Apply(record ConnectRecord) (*ConnectRecord, error) {
	if record.Value == nil {
		return &record, nil
	}

	newValue := make(map[string]interface{})

	// Includes 필터
	for k, v := range record.Value {
		if len(t.Includes) > 0 {
			found := false
			for _, inc := range t.Includes {
				if inc == k {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		// Excludes 필터
		excluded := false
		for _, exc := range t.Excludes {
			if exc == k {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}

		// Renames
		newKey := k
		if renamed, ok := t.Renames[k]; ok {
			newKey = renamed
		}
		newValue[newKey] = v
	}

	record.Value = newValue
	return &record, nil
}

func (t *ReplaceField) Name() string { return "ReplaceField" }

// --- MaskField: 필드 마스킹 ---
// 실제: connect/transforms/src/main/java/.../MaskField.java

type MaskField struct {
	Fields    []string
	MaskValue interface{} // nil이면 타입별 기본값 사용
}

func (t *MaskField) Apply(record ConnectRecord) (*ConnectRecord, error) {
	if record.Value == nil {
		return &record, nil
	}
	for _, field := range t.Fields {
		if _, exists := record.Value[field]; exists {
			if t.MaskValue != nil {
				record.Value[field] = t.MaskValue
			} else {
				record.Value[field] = "***MASKED***"
			}
		}
	}
	return &record, nil
}

func (t *MaskField) Name() string { return "MaskField" }

// --- TimestampRouter: 타임스탬프 기반 토픽 라우팅 ---
// 실제: connect/transforms/src/main/java/.../TimestampRouter.java

type TimestampRouter struct {
	TopicFormat    string // 예: "${topic}-${timestamp}"
	TimestampFormat string // 예: "20060102"
}

func (t *TimestampRouter) Apply(record ConnectRecord) (*ConnectRecord, error) {
	ts := record.Timestamp.Format(t.TimestampFormat)
	newTopic := strings.ReplaceAll(t.TopicFormat, "${topic}", record.Topic)
	newTopic = strings.ReplaceAll(newTopic, "${timestamp}", ts)
	record.Topic = newTopic
	return &record, nil
}

func (t *TimestampRouter) Name() string { return "TimestampRouter" }

// --- RegexRouter: 정규식 기반 토픽 라우팅 ---

type RegexRouter struct {
	Regex       string
	Replacement string
}

func (t *RegexRouter) Apply(record ConnectRecord) (*ConnectRecord, error) {
	re, err := regexp.Compile(t.Regex)
	if err != nil {
		return nil, fmt.Errorf("invalid regex: %v", err)
	}
	record.Topic = re.ReplaceAllString(record.Topic, t.Replacement)
	return &record, nil
}

func (t *RegexRouter) Name() string { return "RegexRouter" }

// --- ValueToKey: Value 필드를 Key로 추출 ---

type ValueToKey struct {
	Fields []string
}

func (t *ValueToKey) Apply(record ConnectRecord) (*ConnectRecord, error) {
	if record.Value == nil {
		return &record, nil
	}
	keyParts := make([]string, 0)
	for _, field := range t.Fields {
		if v, ok := record.Value[field]; ok {
			keyParts = append(keyParts, fmt.Sprintf("%v", v))
		}
	}
	record.Key = strings.Join(keyParts, "-")
	return &record, nil
}

func (t *ValueToKey) Name() string { return "ValueToKey" }

// --- TransformChain: Transform 체인 ---

type TransformChain struct {
	transforms []Transform
}

func NewTransformChain(transforms ...Transform) *TransformChain {
	return &TransformChain{transforms: transforms}
}

func (tc *TransformChain) Apply(record ConnectRecord) (*ConnectRecord, error) {
	current := &record
	for _, t := range tc.transforms {
		result, err := t.Apply(*current)
		if err != nil {
			return nil, fmt.Errorf("transform %s failed: %v", t.Name(), err)
		}
		current = result
	}
	return current, nil
}

// --- FileStreamSource: 파일 소스 커넥터 ---
// 실제: connect/file/src/main/java/.../FileStreamSourceTask.java

type FileStreamSource struct {
	Lines     []string
	Topic     string
	offset    int
}

func NewFileStreamSource(lines []string, topic string) *FileStreamSource {
	return &FileStreamSource{Lines: lines, Topic: topic}
}

func (fs *FileStreamSource) Poll() []ConnectRecord {
	var records []ConnectRecord
	for fs.offset < len(fs.Lines) {
		line := fs.Lines[fs.offset]
		records = append(records, ConnectRecord{
			Topic: fs.Topic,
			Value: map[string]interface{}{"line": line},
			Timestamp: time.Now(),
			Offset: int64(fs.offset),
		})
		fs.offset++
	}
	return records
}

// --- FileStreamSink: 파일 싱크 커넥터 ---
// 실제: connect/file/src/main/java/.../FileStreamSinkTask.java

type FileStreamSink struct {
	Output []string
}

func NewFileStreamSink() *FileStreamSink {
	return &FileStreamSink{}
}

func (fs *FileStreamSink) Put(records []ConnectRecord) {
	for _, r := range records {
		line := fmt.Sprintf("%v", r.Value)
		fs.Output = append(fs.Output, line)
	}
}

// --- Dead Letter Queue ---

type DeadLetterQueue struct {
	Records []ConnectRecord
	Errors  []error
}

func NewDeadLetterQueue() *DeadLetterQueue {
	return &DeadLetterQueue{}
}

func (dlq *DeadLetterQueue) Send(record ConnectRecord, err error) {
	dlq.Records = append(dlq.Records, record)
	dlq.Errors = append(dlq.Errors, err)
}

// --- Converter 시뮬레이션 ---

type Converter interface {
	FromConnectData(record ConnectRecord) string
	ToConnectData(data string) ConnectRecord
}

type JSONConverter struct{}

func (jc *JSONConverter) FromConnectData(record ConnectRecord) string {
	parts := []string{}
	for k, v := range record.Value {
		parts = append(parts, fmt.Sprintf(`"%s":"%v"`, k, v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func (jc *JSONConverter) ToConnectData(data string) ConnectRecord {
	return ConnectRecord{
		Value: map[string]interface{}{"raw": data},
	}
}

// --- 메인 함수 ---

func main() {
	fmt.Println("=== Kafka Connect Transforms + File Connector 시뮬레이션 ===")
	fmt.Println()

	// 1. 개별 Transform 테스트
	fmt.Println("--- 1단계: 개별 Transform 테스트 ---")

	record := ConnectRecord{
		Topic: "raw-events",
		Key:   "key-1",
		Value: map[string]interface{}{
			"user_id":    "user-123",
			"email":      "user@example.com",
			"password":   "secret123",
			"event_type": "login",
			"ip_address": "192.168.1.1",
		},
		Headers:   map[string]string{"source": "web"},
		Timestamp: time.Date(2026, 3, 8, 10, 30, 0, 0, time.UTC),
	}
	fmt.Printf("  원본: %s\n", record)

	// InsertField
	insertTransform := &InsertField{
		StaticField: "environment",
		StaticValue: "production",
		TimestampField: "processed_at",
	}
	result, _ := insertTransform.Apply(record)
	fmt.Printf("  InsertField 후: environment=%v, processed_at=%v\n",
		result.Value["environment"], result.Value["processed_at"])

	// MaskField
	maskTransform := &MaskField{Fields: []string{"password", "email"}}
	result, _ = maskTransform.Apply(*result)
	fmt.Printf("  MaskField 후: password=%v, email=%v\n",
		result.Value["password"], result.Value["email"])

	// ReplaceField
	replaceTransform := &ReplaceField{
		Renames:  map[string]string{"user_id": "userId", "event_type": "eventType"},
		Excludes: []string{"ip_address"},
	}
	result, _ = replaceTransform.Apply(*result)
	fmt.Printf("  ReplaceField 후: %v\n", result.Value)

	// 2. Transform 체인
	fmt.Println()
	fmt.Println("--- 2단계: Transform 체인 (파이프라인) ---")
	chain := NewTransformChain(
		&InsertField{StaticField: "cluster", StaticValue: "prod-us-east"},
		&MaskField{Fields: []string{"password"}},
		&ReplaceField{Excludes: []string{"ip_address"}},
		&ValueToKey{Fields: []string{"user_id"}},
	)

	originalRecord := ConnectRecord{
		Topic: "events",
		Value: map[string]interface{}{
			"user_id":    "user-456",
			"password":   "pass789",
			"action":     "purchase",
			"ip_address": "10.0.0.1",
		},
		Timestamp: time.Now(),
	}

	transformed, err := chain.Apply(originalRecord)
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
	} else {
		fmt.Printf("  변환 전: %s\n", originalRecord)
		fmt.Printf("  변환 후: %s\n", transformed)
	}

	// 3. TimestampRouter
	fmt.Println()
	fmt.Println("--- 3단계: TimestampRouter (시간 기반 토픽 라우팅) ---")
	tsRouter := &TimestampRouter{
		TopicFormat:    "${topic}-${timestamp}",
		TimestampFormat: "20060102",
	}

	dates := []time.Time{
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC),
	}
	for _, d := range dates {
		rec := ConnectRecord{Topic: "logs", Timestamp: d, Value: map[string]interface{}{"msg": "test"}}
		routed, _ := tsRouter.Apply(rec)
		fmt.Printf("  %s → %s\n", d.Format("2006-01-02"), routed.Topic)
	}

	// 4. RegexRouter
	fmt.Println()
	fmt.Println("--- 4단계: RegexRouter (정규식 기반 토픽 라우팅) ---")
	regexRouter := &RegexRouter{
		Regex:       "^(.*)-raw$",
		Replacement: "${1}-processed",
	}
	testTopics := []string{"orders-raw", "users-raw", "events-raw", "other-topic"}
	for _, topic := range testTopics {
		rec := ConnectRecord{Topic: topic, Value: map[string]interface{}{}}
		routed, _ := regexRouter.Apply(rec)
		fmt.Printf("  %s → %s\n", topic, routed.Topic)
	}

	// 5. FileStreamSource + Transform + FileStreamSink
	fmt.Println()
	fmt.Println("--- 5단계: FileStream Source → Transform → Sink 파이프라인 ---")
	source := NewFileStreamSource([]string{
		"2026-03-08 user-1 login success",
		"2026-03-08 user-2 purchase item-42",
		"2026-03-08 user-1 logout",
		"2026-03-08 admin delete-user user-99",
	}, "file-events")

	sink := NewFileStreamSink()

	// Transform: 라인 파싱은 단순화
	fileTransform := NewTransformChain(
		&InsertField{StaticField: "source", StaticValue: "file-connector"},
	)

	records := source.Poll()
	fmt.Printf("  소스에서 읽은 레코드: %d\n", len(records))

	var transformedRecords []ConnectRecord
	for _, r := range records {
		t, err := fileTransform.Apply(r)
		if err != nil {
			fmt.Printf("  변환 에러: %v\n", err)
			continue
		}
		transformedRecords = append(transformedRecords, *t)
	}

	sink.Put(transformedRecords)
	fmt.Printf("  싱크에 기록된 레코드: %d\n", len(sink.Output))
	for i, line := range sink.Output {
		fmt.Printf("    [%d] %s\n", i, line)
	}

	// 6. Dead Letter Queue
	fmt.Println()
	fmt.Println("--- 6단계: Dead Letter Queue ---")
	dlq := NewDeadLetterQueue()

	// 에러를 발생시키는 Transform
	badRecord := ConnectRecord{
		Topic: "events",
		Value: nil, // nil value → 일부 transform에서 에러 가능
	}

	// 체인에서 에러 발생 시 DLQ로 전송
	strictChain := NewTransformChain(
		&ReplaceField{Includes: []string{"required_field"}},
	)
	_, err = strictChain.Apply(badRecord)
	if err != nil {
		dlq.Send(badRecord, err)
		fmt.Printf("  DLQ에 전송: %v\n", err)
	} else {
		fmt.Printf("  변환 성공 (예상치 못함)\n")
	}

	// 수동 DLQ 테스트
	dlq.Send(ConnectRecord{Topic: "events", Value: map[string]interface{}{"bad": true}},
		fmt.Errorf("serialization error"))
	fmt.Printf("  DLQ 레코드 수: %d\n", len(dlq.Records))
	for i, e := range dlq.Errors {
		fmt.Printf("    [%d] %v\n", i, e)
	}

	// 7. Converter 시뮬레이션
	fmt.Println()
	fmt.Println("--- 7단계: Converter (JSON) ---")
	converter := &JSONConverter{}
	jsonRecord := ConnectRecord{
		Topic: "test",
		Value: map[string]interface{}{
			"name": "Alice",
			"age":  30,
		},
	}
	json := converter.FromConnectData(jsonRecord)
	fmt.Printf("  ConnectRecord → JSON: %s\n", json)
	back := converter.ToConnectData(json)
	fmt.Printf("  JSON → ConnectRecord: %v\n", back.Value)

	// 요약
	fmt.Println()
	fmt.Println("=== 요약 ===")
	fmt.Println("  - SMT 체인: InsertField → MaskField → ReplaceField → ValueToKey")
	fmt.Println("  - TimestampRouter: 타임스탬프 기반 토픽 파티셔닝 (일별/주별)")
	fmt.Println("  - RegexRouter: 정규식 기반 토픽 라우팅")
	fmt.Println("  - FileStreamSource/Sink: 파일 입출력 커넥터")
	fmt.Println("  - Dead Letter Queue: 변환 실패 레코드 격리")
	fmt.Println("  - Converter: Connect 내부 ↔ 직렬화 포맷 변환")
}
