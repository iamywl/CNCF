package main

import (
	"fmt"
	"strings"
)

// =============================================================================
// Jaeger → OTel 환경변수 마이그레이션 시뮬레이션
// =============================================================================
//
// Jaeger v2는 OpenTelemetry Collector 기반으로 재작성되어,
// 기존 Jaeger 환경변수(JAEGER_*)를 OTel 환경변수(OTEL_*)로 마이그레이션해야 한다.
//
// 핵심 개념:
//   - 1:1 매핑: JAEGER_AGENT_HOST → OTEL_EXPORTER_OTLP_ENDPOINT
//   - 값 변환: 포트 번호 변경, 프로토콜 접두사 추가
//   - 폐기(Deprecated): 대응하는 OTel 변수가 없는 경우
//   - 경고 생성: 호환되지 않는 설정 감지
//
// 실제 코드 참조:
//   - cmd/jaeger/internal/migration/: 마이그레이션 유틸리티
// =============================================================================

// --- 환경변수 매핑 규칙 ---

type MigrationAction int

const (
	ActionMap       MigrationAction = iota // 1:1 매핑
	ActionTransform                        // 값 변환 필요
	ActionDeprecate                        // 폐기됨
	ActionIgnore                           // OTel에서 불필요
	ActionManual                           // 수동 마이그레이션 필요
)

func (a MigrationAction) String() string {
	return []string{"MAP", "TRANSFORM", "DEPRECATED", "IGNORE", "MANUAL"}[a]
}

type MappingRule struct {
	JaegerVar   string
	OTelVar     string
	Action      MigrationAction
	Transform   func(string) string // 값 변환 함수
	Description string
}

// --- 마이그레이션 결과 ---

type MigrationResult struct {
	Original    string
	Value       string
	NewVar      string
	NewValue    string
	Action      MigrationAction
	Warning     string
	Description string
}

// --- 마이그레이션 엔진 ---

type MigrationEngine struct {
	rules []MappingRule
}

func NewMigrationEngine() *MigrationEngine {
	engine := &MigrationEngine{}
	engine.registerRules()
	return engine
}

func (e *MigrationEngine) registerRules() {
	e.rules = []MappingRule{
		// Exporter 설정
		{
			JaegerVar: "JAEGER_AGENT_HOST",
			OTelVar:   "OTEL_EXPORTER_OTLP_ENDPOINT",
			Action:    ActionTransform,
			Transform: func(v string) string {
				return "http://" + v + ":4318"
			},
			Description: "에이전트 호스트 → OTLP 엔드포인트 (HTTP)",
		},
		{
			JaegerVar: "JAEGER_AGENT_PORT",
			OTelVar:   "",
			Action:    ActionDeprecate,
			Description: "Compact Thrift 포트 → OTLP 프로토콜로 대체",
		},
		{
			JaegerVar: "JAEGER_ENDPOINT",
			OTelVar:   "OTEL_EXPORTER_OTLP_ENDPOINT",
			Action:    ActionTransform,
			Transform: func(v string) string {
				// http://host:14268/api/traces → http://host:4318
				v = strings.Replace(v, ":14268/api/traces", ":4318", 1)
				v = strings.Replace(v, ":14250", ":4317", 1)
				return v
			},
			Description: "Jaeger HTTP/gRPC 엔드포인트 → OTLP 엔드포인트",
		},
		{
			JaegerVar: "JAEGER_COLLECTOR_ENDPOINT",
			OTelVar:   "OTEL_EXPORTER_OTLP_ENDPOINT",
			Action:    ActionTransform,
			Transform: func(v string) string {
				return strings.Replace(v, ":14268", ":4318", 1)
			},
			Description: "컬렉터 엔드포인트 포트 변환",
		},

		// 서비스/태그
		{
			JaegerVar:   "JAEGER_SERVICE_NAME",
			OTelVar:     "OTEL_SERVICE_NAME",
			Action:      ActionMap,
			Description: "서비스 이름 (직접 매핑)",
		},
		{
			JaegerVar: "JAEGER_TAGS",
			OTelVar:   "OTEL_RESOURCE_ATTRIBUTES",
			Action:    ActionTransform,
			Transform: func(v string) string {
				// key=value,key2=value2 형식은 동일하지만 키 네이밍 규칙 변경
				return v
			},
			Description: "태그 → 리소스 속성",
		},

		// 샘플링
		{
			JaegerVar:   "JAEGER_SAMPLER_TYPE",
			OTelVar:     "OTEL_TRACES_SAMPLER",
			Action:      ActionTransform,
			Transform: func(v string) string {
				switch v {
				case "const":
					return "always_on"
				case "probabilistic":
					return "traceidratio"
				case "ratelimiting":
					return "parentbased_traceidratio"
				case "remote":
					return "jaeger_remote"
				default:
					return v
				}
			},
			Description: "샘플러 타입 변환",
		},
		{
			JaegerVar:   "JAEGER_SAMPLER_PARAM",
			OTelVar:     "OTEL_TRACES_SAMPLER_ARG",
			Action:      ActionMap,
			Description: "샘플러 파라미터 (직접 매핑)",
		},
		{
			JaegerVar:   "JAEGER_SAMPLER_MANAGER_HOST_PORT",
			OTelVar:     "",
			Action:      ActionManual,
			Description: "원격 샘플링 → OTel remote sampler 별도 설정 필요",
		},

		// 전파(Propagation)
		{
			JaegerVar: "JAEGER_PROPAGATION",
			OTelVar:   "OTEL_PROPAGATORS",
			Action:    ActionTransform,
			Transform: func(v string) string {
				switch v {
				case "jaeger":
					return "jaeger"
				case "b3":
					return "b3multi"
				case "w3c":
					return "tracecontext,baggage"
				default:
					return "tracecontext,baggage"
				}
			},
			Description: "전파 형식 변환",
		},

		// 리포터
		{
			JaegerVar:   "JAEGER_REPORTER_LOG_SPANS",
			OTelVar:     "OTEL_LOG_LEVEL",
			Action:      ActionTransform,
			Transform: func(v string) string {
				if v == "true" {
					return "debug"
				}
				return "info"
			},
			Description: "span 로깅 → 로그 레벨",
		},
		{
			JaegerVar:   "JAEGER_REPORTER_MAX_QUEUE_SIZE",
			OTelVar:     "OTEL_BSP_MAX_QUEUE_SIZE",
			Action:      ActionMap,
			Description: "리포터 큐 크기",
		},
		{
			JaegerVar:   "JAEGER_REPORTER_FLUSH_INTERVAL",
			OTelVar:     "OTEL_BSP_SCHEDULE_DELAY",
			Action:      ActionMap,
			Description: "리포터 플러시 간격",
		},

		// 인증
		{
			JaegerVar:   "JAEGER_USER",
			OTelVar:     "",
			Action:      ActionManual,
			Description: "인증 → OTEL_EXPORTER_OTLP_HEADERS로 수동 설정",
		},
		{
			JaegerVar:   "JAEGER_PASSWORD",
			OTelVar:     "",
			Action:      ActionManual,
			Description: "인증 → OTEL_EXPORTER_OTLP_HEADERS로 수동 설정",
		},

		// 기타
		{
			JaegerVar:   "JAEGER_DISABLED",
			OTelVar:     "OTEL_SDK_DISABLED",
			Action:      ActionMap,
			Description: "SDK 비활성화",
		},
	}
}

// Migrate는 환경변수 목록을 마이그레이션한다.
func (e *MigrationEngine) Migrate(envVars map[string]string) []MigrationResult {
	var results []MigrationResult

	for _, rule := range e.rules {
		value, exists := envVars[rule.JaegerVar]
		if !exists {
			continue
		}

		result := MigrationResult{
			Original:    rule.JaegerVar,
			Value:       value,
			Action:      rule.Action,
			Description: rule.Description,
		}

		switch rule.Action {
		case ActionMap:
			result.NewVar = rule.OTelVar
			result.NewValue = value
		case ActionTransform:
			result.NewVar = rule.OTelVar
			if rule.Transform != nil {
				result.NewValue = rule.Transform(value)
			} else {
				result.NewValue = value
			}
		case ActionDeprecate:
			result.Warning = fmt.Sprintf("%s is deprecated, no OTel equivalent", rule.JaegerVar)
		case ActionManual:
			result.Warning = fmt.Sprintf("%s requires manual migration", rule.JaegerVar)
		case ActionIgnore:
			result.Warning = fmt.Sprintf("%s is not needed in OTel", rule.JaegerVar)
		}

		results = append(results, result)
	}

	return results
}

// GenerateOTelEnv는 OTel 환경변수 목록을 생성한다.
func (e *MigrationEngine) GenerateOTelEnv(results []MigrationResult) map[string]string {
	otelEnv := make(map[string]string)
	for _, r := range results {
		if r.NewVar != "" && r.NewValue != "" {
			otelEnv[r.NewVar] = r.NewValue
		}
	}
	return otelEnv
}

func main() {
	fmt.Println("=== Jaeger → OTel 환경변수 마이그레이션 시뮬레이션 ===")
	fmt.Println()

	engine := NewMigrationEngine()

	// --- 매핑 규칙 ---
	fmt.Println("[1] 등록된 매핑 규칙")
	fmt.Println(strings.Repeat("-", 70))
	for _, rule := range engine.rules {
		otel := rule.OTelVar
		if otel == "" {
			otel = "(none)"
		}
		fmt.Printf("  [%-10s] %-40s → %s\n", rule.Action, rule.JaegerVar, otel)
	}
	fmt.Println()

	// --- 마이그레이션 실행 ---
	fmt.Println("[2] 마이그레이션 실행")
	fmt.Println(strings.Repeat("-", 70))

	jaegerEnv := map[string]string{
		"JAEGER_SERVICE_NAME":               "my-service",
		"JAEGER_AGENT_HOST":                 "jaeger-agent.monitoring",
		"JAEGER_AGENT_PORT":                 "6831",
		"JAEGER_ENDPOINT":                   "http://jaeger-collector:14268/api/traces",
		"JAEGER_SAMPLER_TYPE":               "probabilistic",
		"JAEGER_SAMPLER_PARAM":              "0.1",
		"JAEGER_TAGS":                       "environment=production,version=1.0",
		"JAEGER_PROPAGATION":                "w3c",
		"JAEGER_REPORTER_LOG_SPANS":         "true",
		"JAEGER_REPORTER_MAX_QUEUE_SIZE":    "1000",
		"JAEGER_REPORTER_FLUSH_INTERVAL":    "1000",
		"JAEGER_SAMPLER_MANAGER_HOST_PORT":  "jaeger-agent:5778",
		"JAEGER_USER":                       "admin",
		"JAEGER_PASSWORD":                   "secret",
		"JAEGER_DISABLED":                   "false",
	}

	fmt.Println("  입력 (Jaeger 환경변수):")
	for k, v := range jaegerEnv {
		display := v
		if k == "JAEGER_PASSWORD" {
			display = "***"
		}
		fmt.Printf("    %s=%s\n", k, display)
	}
	fmt.Println()

	results := engine.Migrate(jaegerEnv)

	// --- 결과 출력 ---
	fmt.Println("[3] 마이그레이션 결과")
	fmt.Println(strings.Repeat("-", 70))

	warnings := 0
	for _, r := range results {
		icon := "+"
		if r.Warning != "" {
			icon = "!"
			warnings++
		}
		fmt.Printf("  [%s] %s=%s\n", icon, r.Original, r.Value)
		if r.NewVar != "" {
			fmt.Printf("      → %s=%s\n", r.NewVar, r.NewValue)
		}
		fmt.Printf("      (%s: %s)\n", r.Action, r.Description)
		if r.Warning != "" {
			fmt.Printf("      WARNING: %s\n", r.Warning)
		}
	}
	fmt.Println()

	// --- OTel 환경변수 생성 ---
	fmt.Println("[4] 생성된 OTel 환경변수")
	fmt.Println(strings.Repeat("-", 70))

	otelEnv := engine.GenerateOTelEnv(results)
	for k, v := range otelEnv {
		fmt.Printf("  %s=%s\n", k, v)
	}
	fmt.Println()

	// --- 요약 ---
	fmt.Println("[5] 마이그레이션 요약")
	fmt.Println(strings.Repeat("-", 70))
	actionCounts := make(map[MigrationAction]int)
	for _, r := range results {
		actionCounts[r.Action]++
	}
	fmt.Printf("  Total Jaeger vars: %d\n", len(jaegerEnv))
	fmt.Printf("  Migrated:          %d\n", len(results))
	fmt.Printf("  Direct mapped:     %d\n", actionCounts[ActionMap])
	fmt.Printf("  Transformed:       %d\n", actionCounts[ActionTransform])
	fmt.Printf("  Deprecated:        %d\n", actionCounts[ActionDeprecate])
	fmt.Printf("  Manual required:   %d\n", actionCounts[ActionManual])
	fmt.Printf("  Warnings:          %d\n", warnings)
	fmt.Printf("  OTel vars output:  %d\n", len(otelEnv))
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
