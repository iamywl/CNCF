package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// =============================================================================
// Helm Values 병합 PoC
// =============================================================================
//
// 참조: pkg/chart/common/values.go, pkg/strvals/parser.go,
//       pkg/chart/common/util/coalesce.go
//
// Helm의 Values 시스템은 다음 기능을 제공한다:
//   1. YAML Values deep merge — 차트 기본값과 사용자 값의 재귀적 병합
//   2. --set 문자열 파싱 — "a.b.c=value" 형식의 문자열을 중첩 맵으로 변환
//   3. 서브차트 값 전파 — 부모 차트 → 서브차트로 값 전파, global 키 처리
//   4. JSON Schema 검증 — values.schema.json으로 입력값 유효성 검사
// =============================================================================

// --- Values 타입 ---
// Helm 소스: pkg/chart/common/values.go의 Values 타입
// map[string]any 기반으로 YAML 값을 표현
type Values map[string]any

// GlobalKey는 전역 변수 저장 키이다.
// Helm 소스: pkg/chart/common/values.go의 GlobalKey
const GlobalKey = "global"

// --- istable: YAML 테이블(맵) 여부 확인 ---
// Helm 소스: pkg/chart/common/util/coalesce.go의 istable
func istable(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

// --- CoalesceTables: 두 맵을 재귀적으로 병합 ---
// Helm 소스: pkg/chart/common/util/coalesce.go의 coalesceTablesFullKey
//
// 핵심 규칙:
//   - dst(사용자 값)가 src(차트 기본값)보다 우선
//   - 스칼라/배열: dst 값이 src를 덮어씀
//   - 맵: 재귀적으로 병합
//   - nil 값: coalesce 모드에서는 키 삭제, merge 모드에서는 유지
func CoalesceTables(dst, src map[string]any, prefix string, merge bool) map[string]any {
	if src == nil {
		return dst
	}
	if dst == nil {
		return src
	}

	// 원본 src에서 nil이 아닌 키 추적
	srcOriginalNonNil := make(map[string]bool)
	for key, val := range src {
		if val != nil {
			srcOriginalNonNil[key] = true
		}
	}

	// dst의 nil 값을 src에 전파
	for key, val := range dst {
		if val == nil {
			src[key] = nil
		}
	}

	for key, val := range src {
		fullkey := key
		if prefix != "" {
			fullkey = prefix + "." + key
		}

		dv, ok := dst[key]

		// Coalesce 모드: dst에 nil이 있고 src에 non-nil이 있으면 키 삭제
		// 이것이 Helm에서 --set key=null로 기본값을 제거하는 메커니즘
		if ok && !merge && dv == nil && srcOriginalNonNil[key] {
			delete(dst, key)
		} else if !ok {
			// dst에 없는 키는 src에서 복사
			dst[key] = val
		} else if istable(val) {
			if istable(dv) {
				// 양쪽 다 맵이면 재귀 병합
				CoalesceTables(dv.(map[string]any), val.(map[string]any), fullkey, merge)
			} else {
				fmt.Printf("  [경고] %s: 맵을 비맵으로 덮어쓸 수 없음\n", fullkey)
			}
		} else if istable(dv) && val != nil {
			fmt.Printf("  [경고] %s: 목적지가 맵인데 비맵 값 무시\n", fullkey)
		}
		// 스칼라/배열은 dst가 이미 갖고 있으므로 아무것도 안 함 (dst 우선)
	}
	return dst
}

// --- ParseSet: --set 문자열 파싱 ---
// Helm 소스: pkg/strvals/parser.go의 Parse 함수
//
// "a.b.c=value,x.y=123" 형식의 문자열을 중첩 맵으로 변환한다.
// 파싱 규칙:
//   - '.' → 중첩 맵 생성
//   - '=' → 키-값 분리
//   - ',' → 다음 키-값 쌍
//   - '[N]' → 배열 인덱스
//   - 타입 추론: "true"→bool, "123"→int64, "null"→nil, 나머지→string
func ParseSet(s string) (map[string]any, error) {
	vals := map[string]any{}
	if s == "" {
		return vals, nil
	}

	// 쉼표로 분리하되, 이스케이프 처리
	pairs := splitPairs(s)

	for _, pair := range pairs {
		key, value, err := splitKeyValue(pair)
		if err != nil {
			return nil, err
		}
		setNestedValue(vals, key, typedVal(value))
	}

	return vals, nil
}

// splitPairs는 쉼표로 키-값 쌍을 분리한다.
func splitPairs(s string) []string {
	var pairs []string
	var current strings.Builder
	escaped := false

	for _, r := range s {
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == ',' {
			pairs = append(pairs, current.String())
			current.Reset()
			continue
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		pairs = append(pairs, current.String())
	}
	return pairs
}

// splitKeyValue는 "key=value"를 분리한다.
func splitKeyValue(s string) (string, string, error) {
	idx := strings.Index(s, "=")
	if idx < 0 {
		return "", "", fmt.Errorf("키 %q에 값이 없음", s)
	}
	return s[:idx], s[idx+1:], nil
}

// setNestedValue는 점(.)으로 구분된 경로에 값을 설정한다.
// Helm 소스: pkg/strvals/parser.go의 key 메서드 내 '.' 케이스
func setNestedValue(data map[string]any, path string, value any) {
	keys := strings.Split(path, ".")
	current := data

	for i, key := range keys {
		// 배열 인덱스 처리: key[0], key[1] 등
		if bracketIdx := strings.Index(key, "["); bracketIdx >= 0 {
			mapKey := key[:bracketIdx]
			idxStr := key[bracketIdx+1 : len(key)-1]
			idx, _ := strconv.Atoi(idxStr)

			var list []any
			if existing, ok := current[mapKey]; ok {
				list = existing.([]any)
			}
			// 리스트 크기 확장
			for len(list) <= idx {
				list = append(list, nil)
			}

			if i == len(keys)-1 {
				list[idx] = value
			} else {
				if list[idx] == nil {
					list[idx] = map[string]any{}
				}
				current[mapKey] = list
				current = list[idx].(map[string]any)
				continue
			}
			current[mapKey] = list
			return
		}

		if i == len(keys)-1 {
			current[key] = value
			return
		}

		if _, ok := current[key]; !ok {
			current[key] = map[string]any{}
		}
		next, ok := current[key].(map[string]any)
		if !ok {
			// 기존 스칼라 값을 맵으로 교체
			next = map[string]any{}
			current[key] = next
		}
		current = next
	}
}

// typedVal은 문자열을 적절한 Go 타입으로 변환한다.
// Helm 소스: pkg/strvals/parser.go의 typedVal 함수
func typedVal(val string) any {
	if strings.EqualFold(val, "true") {
		return true
	}
	if strings.EqualFold(val, "false") {
		return false
	}
	if strings.EqualFold(val, "null") {
		return nil
	}
	if val == "0" {
		return int64(0)
	}
	// 0으로 시작하지 않는 숫자는 정수로 파싱 시도
	if len(val) > 0 && val[0] != '0' {
		if iv, err := strconv.ParseInt(val, 10, 64); err == nil {
			return iv
		}
	}
	return val
}

// --- CoalesceGlobals: 전역 변수 전파 ---
// Helm 소스: pkg/chart/common/util/coalesce.go의 coalesceGlobals
//
// 부모 차트의 global 키를 서브차트의 global에 병합한다.
// 서브차트의 기존 global이 부모보다 우선한다.
func CoalesceGlobals(dest, src map[string]any) {
	var dg, sg map[string]any

	// 목적지(서브차트)의 global
	if destGlob, ok := dest[GlobalKey]; !ok {
		dg = make(map[string]any)
	} else if dg, ok = destGlob.(map[string]any); !ok {
		return
	}

	// 소스(부모 차트)의 global
	if srcGlob, ok := src[GlobalKey]; !ok {
		sg = make(map[string]any)
	} else if sg, ok = srcGlob.(map[string]any); !ok {
		return
	}

	// 부모 global을 서브차트 global에 병합 (서브차트 우선)
	for key, val := range sg {
		if _, ok := dg[key]; !ok {
			dg[key] = val
		} else if istable(val) && istable(dg[key]) {
			// 양쪽 다 맵이면 재귀 병합 (부모→서브, 서브 우선)
			CoalesceTables(val.(map[string]any), dg[key].(map[string]any), "", true)
			dg[key] = val
		}
	}
	dest[GlobalKey] = dg
}

// --- JSONSchema 검증 ---
// Helm 소스: pkg/chart/common/util/jsonschema.go
//
// values.schema.json으로 입력값의 타입, 필수 필드, 범위를 검증한다.
type SchemaProperty struct {
	Type     string          // "string", "integer", "boolean", "object", "array"
	Required bool            // 필수 여부
	Minimum  *float64        // 최소값 (integer/number용)
	Maximum  *float64        // 최대값
	Enum     []any           // 허용 값 목록
	Props    SchemaValidator // 중첩 속성 (object용)
}

// SchemaValidator는 JSON Schema 검증기이다.
type SchemaValidator map[string]SchemaProperty

// Validate는 values가 스키마를 만족하는지 검증한다.
func (sv SchemaValidator) Validate(values map[string]any) []string {
	var errors []string

	for key, prop := range sv {
		val, exists := values[key]

		// 필수 필드 검사
		if prop.Required && !exists {
			errors = append(errors, fmt.Sprintf("필수 필드 누락: %s", key))
			continue
		}
		if !exists {
			continue
		}

		// 타입 검사
		if err := validateType(key, val, prop); err != "" {
			errors = append(errors, err)
			continue
		}

		// 범위 검사 (숫자)
		if prop.Minimum != nil || prop.Maximum != nil {
			if num, ok := toFloat64(val); ok {
				if prop.Minimum != nil && num < *prop.Minimum {
					errors = append(errors, fmt.Sprintf("%s: 값 %v이 최소값 %v 미만", key, val, *prop.Minimum))
				}
				if prop.Maximum != nil && num > *prop.Maximum {
					errors = append(errors, fmt.Sprintf("%s: 값 %v이 최대값 %v 초과", key, val, *prop.Maximum))
				}
			}
		}

		// 열거형 검사
		if len(prop.Enum) > 0 {
			found := false
			for _, e := range prop.Enum {
				if fmt.Sprint(val) == fmt.Sprint(e) {
					found = true
					break
				}
			}
			if !found {
				errors = append(errors, fmt.Sprintf("%s: 값 %v이 허용 목록 %v에 없음", key, val, prop.Enum))
			}
		}

		// 중첩 속성 검사
		if prop.Props != nil {
			if m, ok := val.(map[string]any); ok {
				subErrors := prop.Props.Validate(m)
				errors = append(errors, subErrors...)
			}
		}
	}

	return errors
}

func validateType(key string, val any, prop SchemaProperty) string {
	if val == nil {
		return ""
	}
	switch prop.Type {
	case "string":
		if _, ok := val.(string); !ok {
			return fmt.Sprintf("%s: string 타입이어야 하지만 %T", key, val)
		}
	case "integer":
		switch val.(type) {
		case int, int64, float64:
		default:
			return fmt.Sprintf("%s: integer 타입이어야 하지만 %T", key, val)
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			return fmt.Sprintf("%s: boolean 타입이어야 하지만 %T", key, val)
		}
	case "object":
		if _, ok := val.(map[string]any); !ok {
			return fmt.Sprintf("%s: object 타입이어야 하지만 %T", key, val)
		}
	}
	return ""
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// --- 유틸리티: JSON 포맷 출력 ---
func prettyJSON(v any) string {
	b, _ := json.MarshalIndent(v, "    ", "  ")
	return string(b)
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║              Helm Values 병합 PoC                            ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("참조: pkg/chart/common/values.go, pkg/strvals/parser.go,")
	fmt.Println("      pkg/chart/common/util/coalesce.go")
	fmt.Println()

	// =================================================================
	// 1. Deep Merge 기본 동작
	// =================================================================
	fmt.Println("1. YAML Values Deep Merge")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("규칙: 사용자 값(dst) > 차트 기본값(src)")
	fmt.Println("      맵은 재귀 병합, 스칼라/배열은 덮어쓰기")

	chartDefaults := map[string]any{
		"replicaCount": int64(1),
		"image": map[string]any{
			"repository": "nginx",
			"tag":        "latest",
			"pullPolicy": "IfNotPresent",
		},
		"service": map[string]any{
			"type": "ClusterIP",
			"port": int64(80),
		},
		"resources": map[string]any{
			"limits": map[string]any{
				"cpu":    "100m",
				"memory": "128Mi",
			},
		},
	}

	userValues := map[string]any{
		"replicaCount": int64(3), // 스칼라 덮어쓰기
		"image": map[string]any{
			"tag": "v2.0.0", // 맵 내 일부만 변경
		},
		"ingress": map[string]any{ // 새 키 추가는 src에서만 가능
			"enabled": true,
		},
	}

	fmt.Printf("\n  차트 기본값:\n    %s\n", prettyJSON(chartDefaults))
	fmt.Printf("\n  사용자 값:\n    %s\n", prettyJSON(userValues))

	result := CoalesceTables(userValues, chartDefaults, "", false)
	fmt.Printf("\n  병합 결과 (사용자 우선):\n    %s\n", prettyJSON(result))

	// =================================================================
	// 2. --set 문자열 파싱
	// =================================================================
	fmt.Println("\n2. --set 문자열 파싱")
	fmt.Println(strings.Repeat("-", 60))

	testCases := []struct {
		input string
		desc  string
	}{
		{"image.tag=v3.0.0", "점(.) 구분 중첩 경로"},
		{"replicaCount=5", "정수 타입 추론"},
		{"service.enabled=true", "불리언 타입 추론"},
		{"image.repository=nginx,image.tag=stable", "쉼표로 복수 값"},
		{"nodeSelector.kubernetes\\.io/os=linux", "이스케이프된 점"},
		{"tolerations[0].key=node-role,tolerations[0].effect=NoSchedule", "배열 인덱스"},
		{"config=null", "null 값 (키 삭제용)"},
	}

	for _, tc := range testCases {
		parsed, err := ParseSet(tc.input)
		if err != nil {
			fmt.Printf("  --set %s\n    오류: %v\n\n", tc.input, err)
			continue
		}
		fmt.Printf("  --set %s\n    설명: %s\n    결과: %s\n\n", tc.input, tc.desc, prettyJSON(parsed))
	}

	// =================================================================
	// 3. null 값으로 기본값 제거 (Coalesce vs Merge)
	// =================================================================
	fmt.Println("3. null 값 처리: Coalesce vs Merge")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("Coalesce: null 값이면 해당 키를 제거 (--set key=null)")
	fmt.Println("Merge:    null 값을 유지 (중간 처리 단계에서 사용)")

	defaults := map[string]any{
		"tolerations": []any{"node-role=master"},
		"nodeAffinity": map[string]any{
			"zone": "us-east-1a",
		},
		"debug": true,
	}

	// Coalesce 모드: null로 기본값 제거
	coalesceUser := map[string]any{
		"tolerations": nil, // 기본 tolerations 제거
		"debug":       nil, // 기본 debug 제거
	}
	coalesceCopy := map[string]any{
		"tolerations": nil,
		"debug":       nil,
	}
	defaultsCopy1 := copyMap(defaults)
	CoalesceTables(coalesceCopy, defaultsCopy1, "", false)
	fmt.Printf("\n  Coalesce 모드 (--set tolerations=null,debug=null):\n    %s\n", prettyJSON(coalesceCopy))

	// Merge 모드: null 유지
	mergeCopy := copyMap(coalesceUser)
	defaultsCopy2 := copyMap(defaults)
	CoalesceTables(mergeCopy, defaultsCopy2, "", true)
	fmt.Printf("\n  Merge 모드 (null 값 유지):\n    %s\n", prettyJSON(mergeCopy))

	// =================================================================
	// 4. 서브차트 값 전파
	// =================================================================
	fmt.Println("\n4. 서브차트 값 전파 (global 키)")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("부모 차트의 global 값이 모든 서브차트에 전파된다.")
	fmt.Println("서브차트의 로컬 global이 부모보다 우선한다.")

	// 부모 차트 값
	parentValues := map[string]any{
		"global": map[string]any{
			"imageRegistry":  "docker.io",
			"storageClass":   "standard",
			"clusterDomain":  "cluster.local",
		},
		"mysql": map[string]any{
			"auth": map[string]any{
				"rootPassword": "secret123",
			},
		},
	}

	// 서브차트(mysql) 기본값
	mysqlDefaults := map[string]any{
		"global": map[string]any{
			"imageRegistry": "gcr.io",  // 서브차트 로컬이 우선됨
			"pullPolicy":    "Always",  // 서브차트만의 global
		},
		"auth": map[string]any{
			"database": "mydb",
			"username": "admin",
		},
	}

	// 서브차트에 부모의 global 전파
	mysqlValues := map[string]any{
		"auth": map[string]any{
			"rootPassword": "secret123",
		},
	}

	fmt.Printf("\n  부모 차트 값:\n    %s\n", prettyJSON(parentValues))
	fmt.Printf("\n  서브차트(mysql) 기본값:\n    %s\n", prettyJSON(mysqlDefaults))

	// Step 1: 부모의 global을 서브차트에 전파
	CoalesceGlobals(mysqlValues, parentValues)
	fmt.Printf("\n  Step 1 — global 전파 후 서브차트 값:\n    %s\n", prettyJSON(mysqlValues))

	// Step 2: 서브차트 기본값과 병합
	CoalesceTables(mysqlValues, mysqlDefaults, "", false)
	fmt.Printf("\n  Step 2 — 서브차트 기본값 병합 후:\n    %s\n", prettyJSON(mysqlValues))

	// =================================================================
	// 5. JSON Schema 검증
	// =================================================================
	fmt.Println("\n5. JSON Schema 검증")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("values.schema.json으로 입력값을 검증한다.")

	minReplica := float64(1)
	maxReplica := float64(100)
	schema := SchemaValidator{
		"replicaCount": {
			Type:     "integer",
			Required: true,
			Minimum:  &minReplica,
			Maximum:  &maxReplica,
		},
		"image": {
			Type:     "object",
			Required: true,
			Props: SchemaValidator{
				"repository": {Type: "string", Required: true},
				"tag":        {Type: "string", Required: true},
			},
		},
		"service": {
			Type: "object",
			Props: SchemaValidator{
				"type": {
					Type: "string",
					Enum: []any{"ClusterIP", "NodePort", "LoadBalancer"},
				},
				"port": {
					Type: "integer",
				},
			},
		},
	}

	// 유효한 값
	validValues := map[string]any{
		"replicaCount": int64(3),
		"image": map[string]any{
			"repository": "nginx",
			"tag":        "v2.0.0",
		},
		"service": map[string]any{
			"type": "NodePort",
			"port": int64(8080),
		},
	}

	fmt.Printf("\n  유효한 값:\n    %s\n", prettyJSON(validValues))
	errs := schema.Validate(validValues)
	if len(errs) == 0 {
		fmt.Println("  검증 결과: 통과")
	}

	// 유효하지 않은 값
	invalidValues := map[string]any{
		"replicaCount": int64(200), // 최대값 초과
		"image":        "nginx:latest", // object가 아닌 string
		"service": map[string]any{
			"type": "ExternalName", // enum에 없음
		},
	}

	fmt.Printf("\n  유효하지 않은 값:\n    %s\n", prettyJSON(invalidValues))
	errs = schema.Validate(invalidValues)
	if len(errs) > 0 {
		fmt.Println("  검증 오류:")
		for _, e := range errs {
			fmt.Printf("    - %s\n", e)
		}
	}

	// 필수 필드 누락
	missingValues := map[string]any{
		"service": map[string]any{
			"port": int64(80),
		},
	}
	fmt.Printf("\n  필수 필드 누락:\n    %s\n", prettyJSON(missingValues))
	errs = schema.Validate(missingValues)
	if len(errs) > 0 {
		fmt.Println("  검증 오류:")
		for _, e := range errs {
			fmt.Printf("    - %s\n", e)
		}
	}

	// =================================================================
	// 6. 전체 Values 처리 파이프라인
	// =================================================================
	fmt.Println("\n6. 전체 Values 처리 파이프라인")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  Values 처리 흐름 (helm install/upgrade):

  ┌────────────────────────┐
  │  1. 차트 기본값 로드     │  values.yaml
  └──────────┬─────────────┘
             v
  ┌────────────────────────┐
  │  2. -f 파일 병합        │  --values override.yaml
  │     (여러 파일 순차 병합) │  --values prod.yaml
  └──────────┬─────────────┘
             v
  ┌────────────────────────┐
  │  3. --set 값 파싱/병합   │  --set image.tag=v2
  │     --set-string        │  --set-string port=8080
  │     --set-json          │  --set-json config='{"a":1}'
  │     --set-file           │  --set-file ca=./ca.pem
  └──────────┬─────────────┘
             v
  ┌────────────────────────┐
  │  4. JSON Schema 검증    │  values.schema.json
  └──────────┬─────────────┘
             v
  ┌────────────────────────┐
  │  5. 서브차트 값 전파     │  global 키 전파
  │     CoalesceValues      │  부모→자식 재귀
  └──────────┬─────────────┘
             v
  ┌────────────────────────┐
  │  6. 템플릿 렌더링 입력   │  .Values 컨텍스트
  └────────────────────────┘

  병합 우선순위 (높음 → 낮음):
  --set > --set-file > -f (마지막 파일) > -f (첫 파일) > values.yaml
`)

	// 실제 파이프라인 시뮬레이션
	fmt.Println("  파이프라인 시뮬레이션:")

	// Step 1: 차트 기본값
	chartVals := map[string]any{
		"replicaCount": int64(1),
		"image": map[string]any{
			"repository": "myapp",
			"tag":        "latest",
			"pullPolicy": "IfNotPresent",
		},
		"service": map[string]any{
			"type": "ClusterIP",
			"port": int64(80),
		},
	}
	fmt.Printf("  [1] 차트 기본값: %s\n", prettyJSON(chartVals))

	// Step 2: -f prod.yaml 병합
	fileOverrides := map[string]any{
		"replicaCount": int64(3),
		"image": map[string]any{
			"pullPolicy": "Always",
		},
		"resources": map[string]any{
			"limits": map[string]any{
				"cpu":    "500m",
				"memory": "512Mi",
			},
		},
	}
	CoalesceTables(fileOverrides, chartVals, "", false)
	fmt.Printf("\n  [2] -f prod.yaml 병합 후: %s\n", prettyJSON(fileOverrides))

	// Step 3: --set 파싱 및 병합
	setValues, _ := ParseSet("image.tag=v2.1.0,service.type=LoadBalancer")
	CoalesceTables(setValues, fileOverrides, "", false)
	fmt.Printf("\n  [3] --set 병합 후: %s\n", prettyJSON(setValues))

	fmt.Println("\n  최종 Values가 템플릿 렌더링에 .Values로 전달됨")
}

// copyMap은 얕은 복사를 수행한다.
func copyMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		if m, ok := v.(map[string]any); ok {
			dst[k] = copyMap(m)
		} else {
			dst[k] = v
		}
	}
	return dst
}
