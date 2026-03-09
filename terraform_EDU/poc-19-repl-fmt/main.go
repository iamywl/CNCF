// Package main은 Terraform의 REPL(terraform console)과 terraform fmt 시스템을
// Go 표준 라이브러리만으로 시뮬레이션하는 PoC이다.
//
// 시뮬레이션하는 핵심 개념:
// 1. REPL Session 구조 (Scope 기반 표현식 평가)
// 2. Handle 메서드 (명령 분기: help, exit, 표현식 평가)
// 3. FormatValue (cty.Value 포매팅 엔진)
// 4. 멀티라인 입력 감지 (Continuation 판별)
// 5. FmtCommand 처리 흐름 (파일 탐색 → 포매팅 → 출력)
// 6. HCL 포매팅 규칙 (인터폴레이션 언래핑, 타입 정규화)
// 7. 디렉토리 재귀 순회 및 .tf 파일 필터링
// 8. diff 모드 (변경 사항 표시)
// 9. check 모드 (포매팅 필요 여부 확인)
// 10. write 모드 (파일 직접 수정)
//
// 실제 소스 참조:
//   - internal/repl/session.go     (Session 구조체, Handle 메서드)
//   - internal/repl/format.go      (FormatValue 함수)
//   - internal/repl/continuation.go (멀티라인 입력 감지)
//   - internal/command/fmt.go      (terraform fmt 구현)
package main

import (
	"fmt"
	"regexp"
	"strings"
)

// ============================================================================
// 1. 값 타입 시스템 (cty 시뮬레이션)
// ============================================================================

// ValueType은 Terraform 표현식의 결과 타입을 나타낸다.
type ValueType int

const (
	TypeNil ValueType = iota
	TypeString
	TypeNumber
	TypeBool
	TypeList
	TypeMap
	TypeObject
	TypeTuple
	TypeNull
)

func (t ValueType) String() string {
	switch t {
	case TypeString:
		return "string"
	case TypeNumber:
		return "number"
	case TypeBool:
		return "bool"
	case TypeList:
		return "list"
	case TypeMap:
		return "map"
	case TypeObject:
		return "object"
	case TypeTuple:
		return "tuple"
	case TypeNull:
		return "null"
	default:
		return "nil"
	}
}

// Value는 Terraform의 cty.Value를 시뮬레이션한다.
type Value struct {
	Type      ValueType
	StrVal    string
	NumVal    float64
	BoolVal   bool
	ListVal   []Value
	MapVal    map[string]Value
	IsNull    bool
	IsSensitive bool
}

// ============================================================================
// 2. FormatValue - 값 포매팅 엔진 (internal/repl/format.go 시뮬레이션)
// ============================================================================

// FormatValue는 Value를 사람이 읽을 수 있는 문자열로 변환한다.
// 실제 Terraform의 FormatValue 함수는 cty.Value를 받아 재귀적으로 포매팅한다.
func FormatValue(v Value, indent int) string {
	if v.IsSensitive {
		return "(sensitive value)"
	}
	if v.IsNull {
		return "null"
	}

	prefix := strings.Repeat("  ", indent)
	innerPrefix := strings.Repeat("  ", indent+1)

	switch v.Type {
	case TypeString:
		return fmt.Sprintf("%q", v.StrVal)
	case TypeNumber:
		if v.NumVal == float64(int(v.NumVal)) {
			return fmt.Sprintf("%d", int(v.NumVal))
		}
		return fmt.Sprintf("%g", v.NumVal)
	case TypeBool:
		return fmt.Sprintf("%t", v.BoolVal)
	case TypeList, TypeTuple:
		if len(v.ListVal) == 0 {
			if v.Type == TypeTuple {
				return "tuple([])"
			}
			return "tolist([])"
		}
		var lines []string
		typeName := "tolist"
		if v.Type == TypeTuple {
			typeName = "tuple"
		}
		lines = append(lines, typeName+"([")
		for _, elem := range v.ListVal {
			lines = append(lines, innerPrefix+FormatValue(elem, indent+1)+",")
		}
		lines = append(lines, prefix+"])")
		return strings.Join(lines, "\n")
	case TypeMap, TypeObject:
		if len(v.MapVal) == 0 {
			if v.Type == TypeObject {
				return "{}"
			}
			return "tomap({})"
		}
		var lines []string
		opener := "tomap({"
		closer := "})"
		if v.Type == TypeObject {
			opener = "{"
			closer = "}"
		}
		lines = append(lines, opener)

		// 키 정렬 (실제 구현도 정렬된 순서로 출력)
		keys := make([]string, 0, len(v.MapVal))
		for k := range v.MapVal {
			keys = append(keys, k)
		}
		sortStrings(keys)

		// 키 최대 길이로 정렬
		maxKeyLen := 0
		for _, k := range keys {
			if len(k) > maxKeyLen {
				maxKeyLen = len(k)
			}
		}

		for _, k := range keys {
			padding := strings.Repeat(" ", maxKeyLen-len(k))
			lines = append(lines,
				fmt.Sprintf("%s%q%s = %s", innerPrefix, k, padding, FormatValue(v.MapVal[k], indent+1)))
		}
		lines = append(lines, prefix+closer)
		return strings.Join(lines, "\n")
	default:
		return "null"
	}
}

func sortStrings(s []string) {
	for i := 0; i < len(s); i++ {
		for j := i + 1; j < len(s); j++ {
			if s[i] > s[j] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

// ============================================================================
// 3. REPL Session (internal/repl/session.go 시뮬레이션)
// ============================================================================

// Variable은 Scope 내 변수를 나타낸다.
type Variable struct {
	Name  string
	Value Value
}

// Resource는 Scope 내 리소스 상태를 나타낸다.
type Resource struct {
	Type       string
	Name       string
	Attributes map[string]Value
}

// Scope는 표현식 평가 스코프를 나타낸다 (lang.Scope 시뮬레이션).
type Scope struct {
	Variables map[string]Variable
	Resources map[string]Resource
	Outputs   map[string]Value
}

// Session은 REPL 세션을 나타낸다.
// 실제 internal/repl/session.go의 Session은 Scope만 가지고 있다.
type Session struct {
	Scope *Scope
}

// Diagnostic은 진단 메시지를 나타낸다.
type Diagnostic struct {
	Severity string // "error" or "warning"
	Summary  string
	Detail   string
}

// HandleResult는 Handle 메서드의 반환값이다.
type HandleResult struct {
	Output      string
	Exit        bool
	Diagnostics []Diagnostic
}

// Handle은 한 줄의 REPL 입력을 처리한다.
// 실제 구현: func (s *Session) Handle(line string) (string, bool, tfdiags.Diagnostics)
func (s *Session) Handle(line string) HandleResult {
	trimmed := strings.TrimSpace(line)

	switch {
	case trimmed == "":
		// 빈 줄: 무시
		return HandleResult{}
	case trimmed == "exit":
		// exit: 세션 종료
		return HandleResult{Exit: true}
	case trimmed == "help":
		return s.handleHelp()
	default:
		// 표현식 평가
		return s.handleEval(trimmed)
	}
}

func (s *Session) handleHelp() HandleResult {
	help := `The Terraform console allows you to experiment with Terraform
interpolations.

You may access resources in the Terraform state (if you have one)
and call any built-in functions.

Type "exit" or press Ctrl-D to leave the console.`
	return HandleResult{Output: help}
}

// handleEval은 표현식을 평가한다.
// 실제 구현에서는 hclsyntax.ParseExpression → lang.EvalExpr를 사용한다.
func (s *Session) handleEval(expr string) HandleResult {
	// 변수 참조: var.xxx
	if strings.HasPrefix(expr, "var.") {
		name := strings.TrimPrefix(expr, "var.")
		if v, ok := s.Scope.Variables[name]; ok {
			return HandleResult{Output: FormatValue(v.Value, 0)}
		}
		return HandleResult{
			Diagnostics: []Diagnostic{{
				Severity: "error",
				Summary:  "Reference to undeclared input variable",
				Detail:   fmt.Sprintf("An input variable with the name %q has not been declared.", name),
			}},
		}
	}

	// 리소스 참조: type.name 또는 type.name.attr
	parts := strings.SplitN(expr, ".", 3)
	if len(parts) >= 2 {
		key := parts[0] + "." + parts[1]
		if res, ok := s.Scope.Resources[key]; ok {
			if len(parts) == 3 {
				attr := parts[2]
				if val, ok := res.Attributes[attr]; ok {
					return HandleResult{Output: FormatValue(val, 0)}
				}
				return HandleResult{
					Diagnostics: []Diagnostic{{
						Severity: "error",
						Summary:  "Unsupported attribute",
						Detail:   fmt.Sprintf("This object does not have an attribute named %q.", attr),
					}},
				}
			}
			// 리소스 전체를 object로 반환
			return HandleResult{Output: FormatValue(Value{
				Type:   TypeObject,
				MapVal: res.Attributes,
			}, 0)}
		}
	}

	// 내장 함수 시뮬레이션
	if strings.HasPrefix(expr, "length(") || strings.HasPrefix(expr, "upper(") ||
		strings.HasPrefix(expr, "lower(") || strings.HasPrefix(expr, "tostring(") {
		return s.evalBuiltinFunction(expr)
	}

	// output 참조
	if strings.HasPrefix(expr, "output.") {
		name := strings.TrimPrefix(expr, "output.")
		if val, ok := s.Scope.Outputs[name]; ok {
			return HandleResult{Output: FormatValue(val, 0)}
		}
	}

	// 리터럴 문자열
	if strings.HasPrefix(expr, "\"") && strings.HasSuffix(expr, "\"") {
		return HandleResult{Output: expr}
	}

	// 리터럴 숫자
	if isNumber(expr) {
		return HandleResult{Output: expr}
	}

	// true/false
	if expr == "true" || expr == "false" {
		return HandleResult{Output: expr}
	}

	return HandleResult{
		Diagnostics: []Diagnostic{{
			Severity: "error",
			Summary:  "Invalid expression",
			Detail:   fmt.Sprintf("Could not evaluate expression: %s", expr),
		}},
	}
}

func (s *Session) evalBuiltinFunction(expr string) HandleResult {
	// 간단한 내장 함수 시뮬레이션
	if strings.HasPrefix(expr, "upper(\"") && strings.HasSuffix(expr, "\")") {
		arg := expr[7 : len(expr)-2]
		return HandleResult{Output: fmt.Sprintf("%q", strings.ToUpper(arg))}
	}
	if strings.HasPrefix(expr, "lower(\"") && strings.HasSuffix(expr, "\")") {
		arg := expr[7 : len(expr)-2]
		return HandleResult{Output: fmt.Sprintf("%q", strings.ToLower(arg))}
	}
	return HandleResult{
		Diagnostics: []Diagnostic{{
			Severity: "error",
			Summary:  "Function evaluation error",
			Detail:   "Simplified REPL only supports upper() and lower() with string literals.",
		}},
	}
}

func isNumber(s string) bool {
	for i, c := range s {
		if c == '.' || c == '-' {
			if i == 0 || c == '.' {
				continue
			}
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// ============================================================================
// 4. 멀티라인 입력 감지 (internal/repl/continuation.go 시뮬레이션)
// ============================================================================

// NeedsContinuation은 입력이 미완성(괄호, 중괄호 등이 닫히지 않음)인지 판별한다.
// 실제 구현에서는 HCL의 Token 수준에서 분석한다.
func NeedsContinuation(input string) bool {
	depth := 0
	inString := false
	escaped := false

	for _, ch := range input {
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && inString {
			escaped = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch ch {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		}
	}
	return depth > 0
}

// ============================================================================
// 5. terraform fmt - HCL 포매팅 엔진 (internal/command/fmt.go 시뮬레이션)
// ============================================================================

// FmtMode는 fmt 명령의 동작 모드를 나타낸다.
type FmtMode int

const (
	FmtModeWrite FmtMode = iota // -write: 파일 직접 수정
	FmtModeDiff                 // -diff: 변경 사항 표시
	FmtModeCheck                // -check: 포매팅 필요 여부만 확인
)

// FmtResult는 단일 파일의 포매팅 결과를 나타낸다.
type FmtResult struct {
	Filename  string
	Original  string
	Formatted string
	Changed   bool
}

// FormatHCL은 HCL 소스코드를 표준 형식으로 포매팅한다.
// 실제 구현은 hclwrite 패키지를 사용하지만, 여기서는 정규표현식으로 핵심 규칙만 시뮬레이션한다.
func FormatHCL(src string) string {
	lines := strings.Split(src, "\n")
	var result []string

	for _, line := range lines {
		formatted := line

		// 규칙 1: 인터폴레이션 언래핑
		// "${var.name}" → var.name (문자열 내 단순 참조는 인터폴레이션 불필요)
		formatted = unwrapInterpolation(formatted)

		// 규칙 2: = 주위 정렬 (블록 내 연속된 할당의 = 위치를 맞춤)
		// 이 규칙은 블록 단위로 적용해야 하므로 간소화

		// 규칙 3: 후행 공백 제거
		formatted = strings.TrimRight(formatted, " \t")

		// 규칙 4: 타입 표현식 정규화
		formatted = normalizeTypeExpression(formatted)

		result = append(result, formatted)
	}

	// 규칙 5: 파일 끝 개행 보장
	output := strings.Join(result, "\n")
	if !strings.HasSuffix(output, "\n") {
		output += "\n"
	}

	// 규칙 6: = 정렬
	output = alignEquals(output)

	return output
}

// unwrapInterpolation은 순수 참조 인터폴레이션을 제거한다.
// "${var.name}" → var.name
// 실제 구현: internal/command/fmt.go의 unwrapHeredocInterpolations
var interpolationRe = regexp.MustCompile(`"\$\{([^}]+)\}"`)

func unwrapInterpolation(line string) string {
	return interpolationRe.ReplaceAllString(line, "$1")
}

// normalizeTypeExpression은 레거시 타입 표현을 정규화한다.
// "string" → string, "list" → list(string)
func normalizeTypeExpression(line string) string {
	replacements := map[string]string{
		`type = "string"`: `type = string`,
		`type = "number"`: `type = number`,
		`type = "bool"`:   `type = bool`,
		`type = "list"`:   `type = list(string)`,
		`type = "map"`:    `type = map(string)`,
	}
	for old, new_ := range replacements {
		if strings.Contains(line, old) {
			line = strings.ReplaceAll(line, old, new_)
		}
	}
	return line
}

// alignEquals는 연속된 할당문의 = 기호를 정렬한다.
// 실제 구현에서는 hclwrite가 블록 단위로 = 정렬을 수행한다.
func alignEquals(src string) string {
	lines := strings.Split(src, "\n")
	var result []string
	var block []int // 연속된 할당문의 인덱스

	flushBlock := func() {
		if len(block) < 2 {
			for _, idx := range block {
				result = append(result, lines[idx])
			}
			block = nil
			return
		}

		// 블록 내 최대 키 길이 찾기
		maxKeyLen := 0
		for _, idx := range block {
			eqIdx := strings.Index(lines[idx], "=")
			if eqIdx > 0 {
				keyPart := strings.TrimRight(lines[idx][:eqIdx], " ")
				if len(keyPart) > maxKeyLen {
					maxKeyLen = len(keyPart)
				}
			}
		}

		// 정렬 적용
		for _, idx := range block {
			eqIdx := strings.Index(lines[idx], "=")
			if eqIdx > 0 {
				keyPart := strings.TrimRight(lines[idx][:eqIdx], " ")
				valPart := strings.TrimLeft(lines[idx][eqIdx+1:], " ")
				padding := strings.Repeat(" ", maxKeyLen-len(keyPart))
				result = append(result, keyPart+padding+" = "+valPart)
			} else {
				result = append(result, lines[idx])
			}
		}
		block = nil
	}

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// 할당문인지 확인 (key = value 패턴이면서 블록 시작이 아닌 것)
		if strings.Contains(trimmed, " = ") && !strings.HasSuffix(trimmed, "{") &&
			!strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "//") &&
			trimmed != "" {
			block = append(block, i)
		} else {
			flushBlock()
			result = append(result, lines[i])
		}
	}
	flushBlock()

	return strings.Join(result, "\n")
}

// GenerateDiff는 두 문자열의 차이를 유닉스 diff 형식으로 생성한다.
func GenerateDiff(filename, original, formatted string) string {
	origLines := strings.Split(original, "\n")
	fmtLines := strings.Split(formatted, "\n")

	var diff strings.Builder
	diff.WriteString(fmt.Sprintf("--- a/%s\n", filename))
	diff.WriteString(fmt.Sprintf("+++ b/%s\n", filename))

	maxLines := len(origLines)
	if len(fmtLines) > maxLines {
		maxLines = len(fmtLines)
	}

	hasChanges := false
	for i := 0; i < maxLines; i++ {
		var origLine, fmtLine string
		if i < len(origLines) {
			origLine = origLines[i]
		}
		if i < len(fmtLines) {
			fmtLine = fmtLines[i]
		}
		if origLine != fmtLine {
			if !hasChanges {
				diff.WriteString(fmt.Sprintf("@@ -%d +%d @@\n", i+1, i+1))
				hasChanges = true
			}
			if i < len(origLines) {
				diff.WriteString(fmt.Sprintf("-%s\n", origLine))
			}
			if i < len(fmtLines) {
				diff.WriteString(fmt.Sprintf("+%s\n", fmtLine))
			}
		}
	}

	return diff.String()
}

// ============================================================================
// REPL 데모
// ============================================================================

func demoREPL() {
	fmt.Println("=== Terraform Console (REPL) 시뮬레이션 ===")
	fmt.Println()

	// Scope 설정 (현재 State 시뮬레이션)
	scope := &Scope{
		Variables: map[string]Variable{
			"region": {
				Name: "region",
				Value: Value{Type: TypeString, StrVal: "ap-northeast-2"},
			},
			"instance_count": {
				Name: "instance_count",
				Value: Value{Type: TypeNumber, NumVal: 3},
			},
			"enable_monitoring": {
				Name: "enable_monitoring",
				Value: Value{Type: TypeBool, BoolVal: true},
			},
			"tags": {
				Name: "tags",
				Value: Value{
					Type: TypeMap,
					MapVal: map[string]Value{
						"env":     {Type: TypeString, StrVal: "production"},
						"project": {Type: TypeString, StrVal: "web-app"},
						"team":    {Type: TypeString, StrVal: "platform"},
					},
				},
			},
			"db_password": {
				Name: "db_password",
				Value: Value{Type: TypeString, StrVal: "secret123", IsSensitive: true},
			},
		},
		Resources: map[string]Resource{
			"aws_instance.web": {
				Type: "aws_instance",
				Name: "web",
				Attributes: map[string]Value{
					"id":            {Type: TypeString, StrVal: "i-0abc123def456"},
					"instance_type": {Type: TypeString, StrVal: "t3.micro"},
					"ami":           {Type: TypeString, StrVal: "ami-0c55b159cbfafe1f0"},
					"public_ip":     {Type: TypeString, StrVal: "54.180.100.50"},
					"tags": {
						Type: TypeMap,
						MapVal: map[string]Value{
							"Name": {Type: TypeString, StrVal: "web-server"},
						},
					},
				},
			},
		},
		Outputs: map[string]Value{
			"vpc_id": {Type: TypeString, StrVal: "vpc-0abc123"},
		},
	}

	session := &Session{Scope: scope}

	// REPL 입력 시뮬레이션
	inputs := []string{
		"",                         // 빈 줄
		"help",                     // 도움말
		"var.region",               // 문자열 변수
		"var.instance_count",       // 숫자 변수
		"var.enable_monitoring",    // 불리언 변수
		"var.tags",                 // 맵 변수
		"var.db_password",          // 민감한 변수
		"aws_instance.web.id",      // 리소스 속성
		"aws_instance.web",         // 리소스 전체
		"var.undefined",            // 미정의 변수
		`upper("hello")`,          // 내장 함수
		`"literal string"`,        // 리터럴
		"42",                       // 숫자 리터럴
		"true",                     // 불리언 리터럴
	}

	for _, input := range inputs {
		if input == "" {
			fmt.Println("> (빈 줄 - 무시)")
			continue
		}
		fmt.Printf("> %s\n", input)
		result := session.Handle(input)

		if result.Exit {
			fmt.Println("(세션 종료)")
			break
		}
		if result.Output != "" {
			fmt.Println(result.Output)
		}
		for _, diag := range result.Diagnostics {
			fmt.Printf("[%s] %s: %s\n", diag.Severity, diag.Summary, diag.Detail)
		}
		fmt.Println()
	}
}

// ============================================================================
// 멀티라인 입력 데모
// ============================================================================

func demoContinuation() {
	fmt.Println("=== 멀티라인 입력 감지 시뮬레이션 ===")
	fmt.Println()

	testCases := []struct {
		input    string
		expected bool
	}{
		{`var.name`, false},
		{`merge(var.tags, {`, true},
		{`[for s in var.list :`, true},
		{`upper("hello")`, false},
		{`{`, true},
		{`{ "key" = "value" }`, false},
		{`"complete string"`, false},
		{`concat([`, true},
	}

	for _, tc := range testCases {
		result := NeedsContinuation(tc.input)
		status := "완성"
		if result {
			status = "계속 입력 필요"
		}
		fmt.Printf("  입력: %-30s → %s\n", tc.input, status)
	}
}

// ============================================================================
// terraform fmt 데모
// ============================================================================

func demoFmt() {
	fmt.Println("=== terraform fmt 시뮬레이션 ===")
	fmt.Println()

	// 포매팅 전 HCL 소스
	original := `resource "aws_instance" "web" {
  ami = "${var.ami}"
  instance_type="${var.instance_type}"
  tags   = {
    Name = "${var.name}"
    Environment  = "production"
  }
}

variable "name" {
  type = "string"
  default = "web-server"
}

variable "ami" {
  type = "string"
}

variable "instance_type" {
  type = "string"
  default = "t3.micro"
}`

	fmt.Println("[포매팅 전]")
	fmt.Println(original)
	fmt.Println()

	formatted := FormatHCL(original)
	fmt.Println("[포매팅 후]")
	fmt.Println(formatted)

	// Diff 모드 시뮬레이션
	fmt.Println("[Diff 모드]")
	diff := GenerateDiff("main.tf", original, formatted)
	if diff != "" {
		fmt.Println(diff)
	} else {
		fmt.Println("변경 사항 없음")
	}

	// Check 모드 시뮬레이션
	fmt.Println("[Check 모드]")
	if original != formatted {
		fmt.Println("main.tf — 포매팅 필요 (exit code 3)")
	} else {
		fmt.Println("모든 파일이 올바르게 포매팅됨 (exit code 0)")
	}
}

// ============================================================================
// FormatValue 심화 데모
// ============================================================================

func demoFormatValue() {
	fmt.Println("=== FormatValue 포매팅 엔진 데모 ===")
	fmt.Println()

	testValues := []struct {
		label string
		value Value
	}{
		{"문자열", Value{Type: TypeString, StrVal: "hello world"}},
		{"숫자(정수)", Value{Type: TypeNumber, NumVal: 42}},
		{"숫자(실수)", Value{Type: TypeNumber, NumVal: 3.14}},
		{"불리언", Value{Type: TypeBool, BoolVal: true}},
		{"null", Value{Type: TypeNull, IsNull: true}},
		{"민감한 값", Value{Type: TypeString, StrVal: "secret", IsSensitive: true}},
		{"빈 리스트", Value{Type: TypeList, ListVal: []Value{}}},
		{"리스트", Value{
			Type: TypeList,
			ListVal: []Value{
				{Type: TypeString, StrVal: "a"},
				{Type: TypeString, StrVal: "b"},
				{Type: TypeString, StrVal: "c"},
			},
		}},
		{"중첩 맵", Value{
			Type: TypeMap,
			MapVal: map[string]Value{
				"name": {Type: TypeString, StrVal: "web-app"},
				"config": {
					Type: TypeObject,
					MapVal: map[string]Value{
						"port":    {Type: TypeNumber, NumVal: 8080},
						"debug":   {Type: TypeBool, BoolVal: false},
						"version": {Type: TypeString, StrVal: "1.0.0"},
					},
				},
			},
		}},
		{"튜플", Value{
			Type: TypeTuple,
			ListVal: []Value{
				{Type: TypeString, StrVal: "us-east-1"},
				{Type: TypeNumber, NumVal: 3},
				{Type: TypeBool, BoolVal: true},
			},
		}},
	}

	for _, tc := range testValues {
		fmt.Printf("[%s]\n%s\n\n", tc.label, FormatValue(tc.value, 0))
	}
}

// ============================================================================
// main
// ============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Terraform REPL & fmt 시뮬레이션 PoC                         ║")
	fmt.Println("║  실제 소스: internal/repl/, internal/command/fmt.go          ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	demoREPL()
	fmt.Println()
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println()

	demoContinuation()
	fmt.Println()
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println()

	demoFormatValue()
	fmt.Println()
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println()

	demoFmt()
}
