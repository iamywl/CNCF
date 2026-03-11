package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// =============================================================================
// Jaeger Elasticsearch Mapping Template 생성 시뮬레이션
// =============================================================================
//
// Jaeger는 Elasticsearch에 span/서비스 데이터를 저장할 때
// 인덱스 매핑 템플릿을 생성하여 필드 타입과 인덱스 설정을 정의한다.
//
// 핵심 개념:
//   - Index Template: ES 인덱스 생성 시 자동 적용되는 매핑
//   - Span Mapping: span 데이터의 필드 정의 (nested, keyword 등)
//   - ILM (Index Lifecycle Management): 인덱스 롤오버/삭제 정책
//   - Version-specific: ES 7.x/8.x에 따라 매핑 차이
//
// 실제 코드 참조:
//   - plugin/storage/es/mappings/: 매핑 템플릿
// =============================================================================

// --- ES Mapping Types ---

type FieldMapping struct {
	Type       string                  `json:"type,omitempty"`
	Index      *bool                   `json:"index,omitempty"`
	Enabled    *bool                   `json:"enabled,omitempty"`
	Properties map[string]FieldMapping `json:"properties,omitempty"`
	Fields     map[string]FieldMapping `json:"fields,omitempty"` // multi-field
}

type IndexSettings struct {
	NumberOfShards   int    `json:"number_of_shards"`
	NumberOfReplicas int    `json:"number_of_replicas"`
	Codec            string `json:"codec,omitempty"`
	RefreshInterval  string `json:"refresh_interval,omitempty"`
	MappingTotalFields int  `json:"mapping.total_fields.limit,omitempty"`
}

type IndexTemplate struct {
	IndexPatterns []string               `json:"index_patterns"`
	Priority      int                    `json:"priority,omitempty"`
	Template      TemplateBody           `json:"template"`
	Aliases       map[string]interface{} `json:"aliases,omitempty"`
}

type TemplateBody struct {
	Settings IndexSettings            `json:"settings"`
	Mappings map[string]interface{}   `json:"mappings"`
}

type ILMPolicy struct {
	Phases map[string]ILMPhase `json:"phases"`
}

type ILMPhase struct {
	MinAge  string                 `json:"min_age,omitempty"`
	Actions map[string]interface{} `json:"actions"`
}

// --- Mapping Builder ---

type MappingBuilder struct {
	esVersion    int // 7 or 8
	shards       int
	replicas     int
	indexPrefix   string
	useILM       bool
	ilmPolicyName string
}

func NewMappingBuilder(esVersion, shards, replicas int, prefix string) *MappingBuilder {
	return &MappingBuilder{
		esVersion:  esVersion,
		shards:     shards,
		replicas:   replicas,
		indexPrefix: prefix,
	}
}

func (b *MappingBuilder) WithILM(policyName string) *MappingBuilder {
	b.useILM = true
	b.ilmPolicyName = policyName
	return b
}

func boolPtr(v bool) *bool { return &v }

// BuildSpanMapping은 span 인덱스 매핑을 생성한다.
func (b *MappingBuilder) BuildSpanMapping() IndexTemplate {
	spanProps := map[string]FieldMapping{
		"traceID": {Type: "keyword"},
		"spanID":  {Type: "keyword"},
		"parentSpanID": {Type: "keyword"},
		"operationName": {
			Type: "keyword",
			Fields: map[string]FieldMapping{
				"text": {Type: "text"},
			},
		},
		"serviceName": {Type: "keyword"},
		"startTime":   {Type: "long"},
		"startTimeMillis": {Type: "date", Fields: map[string]FieldMapping{
			"keyword": {Type: "keyword"},
		}},
		"duration": {Type: "long"},
		"flags":    {Type: "integer"},
		"logs": {
			Type: "nested",
			Properties: map[string]FieldMapping{
				"timestamp": {Type: "long"},
				"fields": {
					Type: "nested",
					Properties: map[string]FieldMapping{
						"key":      {Type: "keyword"},
						"value":    {Type: "keyword"},
						"tagType":  {Type: "keyword"},
					},
				},
			},
		},
		"tags": {
			Type: "nested",
			Properties: map[string]FieldMapping{
				"key":     {Type: "keyword"},
				"value":   {Type: "keyword"},
				"tagType": {Type: "keyword"},
			},
		},
		"tag": {
			Type:    "object",
			Enabled: boolPtr(false), // 인덱싱하지 않음
		},
		"process": {
			Properties: map[string]FieldMapping{
				"serviceName": {Type: "keyword"},
				"tags": {
					Type: "nested",
					Properties: map[string]FieldMapping{
						"key":     {Type: "keyword"},
						"value":   {Type: "keyword"},
						"tagType": {Type: "keyword"},
					},
				},
				"tag": {
					Type:    "object",
					Enabled: boolPtr(false),
				},
			},
		},
		"references": {
			Type: "nested",
			Properties: map[string]FieldMapping{
				"refType": {Type: "keyword"},
				"traceID": {Type: "keyword"},
				"spanID":  {Type: "keyword"},
			},
		},
	}

	settings := IndexSettings{
		NumberOfShards:     b.shards,
		NumberOfReplicas:   b.replicas,
		Codec:              "best_compression",
		RefreshInterval:    "5s",
		MappingTotalFields: 2000,
	}

	mappings := map[string]interface{}{
		"dynamic_templates": []map[string]interface{}{
			{
				"span_tags_map": map[string]interface{}{
					"mapping":            map[string]string{"type": "keyword"},
					"path_match":         "tag.*",
				},
			},
			{
				"process_tags_map": map[string]interface{}{
					"mapping":            map[string]string{"type": "keyword"},
					"path_match":         "process.tag.*",
				},
			},
		},
		"properties": spanProps,
	}

	return IndexTemplate{
		IndexPatterns: []string{b.indexPrefix + "-jaeger-span-*"},
		Priority:      100,
		Template: TemplateBody{
			Settings: settings,
			Mappings: mappings,
		},
	}
}

// BuildServiceMapping은 서비스 인덱스 매핑을 생성한다.
func (b *MappingBuilder) BuildServiceMapping() IndexTemplate {
	serviceProps := map[string]FieldMapping{
		"serviceName":   {Type: "keyword"},
		"operationName": {Type: "keyword"},
	}

	return IndexTemplate{
		IndexPatterns: []string{b.indexPrefix + "-jaeger-service-*"},
		Priority:      100,
		Template: TemplateBody{
			Settings: IndexSettings{
				NumberOfShards:   1,
				NumberOfReplicas: b.replicas,
			},
			Mappings: map[string]interface{}{
				"properties": serviceProps,
			},
		},
	}
}

// BuildDependencyMapping은 의존성 인덱스 매핑을 생성한다.
func (b *MappingBuilder) BuildDependencyMapping() IndexTemplate {
	depProps := map[string]FieldMapping{
		"parent":    {Type: "keyword"},
		"child":     {Type: "keyword"},
		"callCount": {Type: "long"},
		"source":    {Type: "keyword"},
		"timestamp": {Type: "date"},
	}

	return IndexTemplate{
		IndexPatterns: []string{b.indexPrefix + "-jaeger-dependencies-*"},
		Priority:      100,
		Template: TemplateBody{
			Settings: IndexSettings{
				NumberOfShards:   1,
				NumberOfReplicas: b.replicas,
			},
			Mappings: map[string]interface{}{
				"properties": depProps,
			},
		},
	}
}

// BuildILMPolicy는 인덱스 라이프사이클 관리 정책을 생성한다.
func (b *MappingBuilder) BuildILMPolicy(maxAge, deleteAge string) ILMPolicy {
	return ILMPolicy{
		Phases: map[string]ILMPhase{
			"hot": {
				Actions: map[string]interface{}{
					"rollover": map[string]interface{}{
						"max_age":  maxAge,
						"max_size": "50gb",
					},
					"set_priority": map[string]int{"priority": 100},
				},
			},
			"warm": {
				MinAge: "2d",
				Actions: map[string]interface{}{
					"forcemerge": map[string]int{"max_num_segments": 1},
					"shrink":     map[string]int{"number_of_shards": 1},
					"set_priority": map[string]int{"priority": 50},
				},
			},
			"delete": {
				MinAge: deleteAge,
				Actions: map[string]interface{}{
					"delete": map[string]interface{}{},
				},
			},
		},
	}
}

func prettyJSON(v interface{}) string {
	data, _ := json.MarshalIndent(v, "  ", "  ")
	return string(data)
}

func main() {
	fmt.Println("=== Jaeger ES Mapping Template 생성 시뮬레이션 ===")
	fmt.Println()

	builder := NewMappingBuilder(8, 5, 1, "prod")

	// --- Span 매핑 ---
	fmt.Println("[1] Span Index Template")
	fmt.Println(strings.Repeat("-", 60))
	spanTemplate := builder.BuildSpanMapping()
	fmt.Printf("  Index Patterns: %v\n", spanTemplate.IndexPatterns)
	fmt.Printf("  Shards: %d, Replicas: %d\n",
		spanTemplate.Template.Settings.NumberOfShards,
		spanTemplate.Template.Settings.NumberOfReplicas)
	fmt.Printf("  Codec: %s\n", spanTemplate.Template.Settings.Codec)
	fmt.Printf("  %s\n", prettyJSON(spanTemplate))
	fmt.Println()

	// --- Service 매핑 ---
	fmt.Println("[2] Service Index Template")
	fmt.Println(strings.Repeat("-", 60))
	serviceTemplate := builder.BuildServiceMapping()
	fmt.Printf("  %s\n", prettyJSON(serviceTemplate))
	fmt.Println()

	// --- Dependency 매핑 ---
	fmt.Println("[3] Dependency Index Template")
	fmt.Println(strings.Repeat("-", 60))
	depTemplate := builder.BuildDependencyMapping()
	fmt.Printf("  %s\n", prettyJSON(depTemplate))
	fmt.Println()

	// --- ILM 정책 ---
	fmt.Println("[4] ILM Policy (7일 보존)")
	fmt.Println(strings.Repeat("-", 60))
	ilmPolicy := builder.BuildILMPolicy("1d", "7d")
	fmt.Printf("  %s\n", prettyJSON(ilmPolicy))
	fmt.Println()

	// --- 인덱스 이름 패턴 ---
	fmt.Println("[5] 생성될 인덱스 이름 예시")
	fmt.Println(strings.Repeat("-", 60))
	indexNames := []string{
		"prod-jaeger-span-2024-01-15",
		"prod-jaeger-span-2024-01-16",
		"prod-jaeger-service-2024-01-15",
		"prod-jaeger-dependencies-2024-01-15",
	}
	for _, name := range indexNames {
		fmt.Printf("  %s\n", name)
	}
	fmt.Println()

	// --- ES 7 vs 8 차이점 ---
	fmt.Println("[6] ES 버전별 차이점")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("  ES 7.x:")
	fmt.Println("    - _doc type 사용")
	fmt.Println("    - index_template API (legacy)")
	fmt.Println("  ES 8.x:")
	fmt.Println("    - type 없음 (typeless)")
	fmt.Println("    - composable index template API")
	fmt.Println("    - data streams 지원")
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
