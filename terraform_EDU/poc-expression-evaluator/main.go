package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// =============================================================================
// Terraform 표현식 평가(Expression Evaluation) 시뮬레이션
// =============================================================================
// Terraform은 HCL 표현식을 평가하여 리소스 속성 값을 결정합니다.
// 실제 코드: internal/lang/ 디렉토리 (특히 eval.go, functions.go)
//
// 핵심 개념:
// 1. Value 타입 시스템 - cty 라이브러리 기반
// 2. 변수 참조 해석 (var.name, local.name)
// 3. 리소스 속성 참조 (aws_vpc.main.id)
// 4. 내장 함수 (upper, lower, length, format, join, lookup, element)
// 5. 조건식 (condition ? true_val : false_val)
// 6. 문자열 보간 ("Hello, ${var.name}!")
// 7. Unknown 값 전파
// =============================================================================

// ─────────────────────────────────────────────────────────────────────────────
// Value 타입 시스템
// ─────────────────────────────────────────────────────────────────────────────

// ValueType은 값의 타입을 나타냅니다.
// 실제: github.com/zclconf/go-cty/cty 패키지
type ValueType int

const (
	TypeNull    ValueType = iota
	TypeUnknown           // 아직 알 수 없는 값 (plan 단계)
	TypeString
	TypeNumber
	TypeBool
	TypeList
	TypeMap
)

func (t ValueType) String() string {
	switch t {
	case TypeNull:
		return "null"
	case TypeUnknown:
		return "unknown"
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
	default:
		return "invalid"
	}
}

// Value는 Terraform의 값을 나타냅니다.
// cty.Value를 단순화한 버전입니다.
type Value struct {
	Type     ValueType
	StrVal   string
	NumVal   float64
	BoolVal  bool
	ListVal  []Value
	MapVal   map[string]Value
}

// 값 생성 헬퍼 함수들

func NullVal() Value {
	return Value{Type: TypeNull}
}

func UnknownVal() Value {
	return Value{Type: TypeUnknown}
}

func StringVal(s string) Value {
	return Value{Type: TypeString, StrVal: s}
}

func NumberVal(n float64) Value {
	return Value{Type: TypeNumber, NumVal: n}
}

func BoolVal(b bool) Value {
	return Value{Type: TypeBool, BoolVal: b}
}

func ListVal(items ...Value) Value {
	return Value{Type: TypeList, ListVal: items}
}

func MapVal(m map[string]Value) Value {
	return Value{Type: TypeMap, MapVal: m}
}

// IsKnown는 값이 알려져 있는지 확인합니다.
func (v Value) IsKnown() bool {
	return v.Type != TypeUnknown
}

// IsNull는 값이 null인지 확인합니다.
func (v Value) IsNull() bool {
	return v.Type == TypeNull
}

// String은 값의 문자열 표현을 반환합니다.
func (v Value) String() string {
	switch v.Type {
	case TypeNull:
		return "null"
	case TypeUnknown:
		return "(known after apply)"
	case TypeString:
		return fmt.Sprintf("%q", v.StrVal)
	case TypeNumber:
		if v.NumVal == math.Floor(v.NumVal) {
			return fmt.Sprintf("%.0f", v.NumVal)
		}
		return fmt.Sprintf("%g", v.NumVal)
	case TypeBool:
		return fmt.Sprintf("%t", v.BoolVal)
	case TypeList:
		var items []string
		for _, item := range v.ListVal {
			items = append(items, item.String())
		}
		return fmt.Sprintf("[%s]", strings.Join(items, ", "))
	case TypeMap:
		var items []string
		for k, val := range v.MapVal {
			items = append(items, fmt.Sprintf("%s = %s", k, val.String()))
		}
		return fmt.Sprintf("{%s}", strings.Join(items, ", "))
	default:
		return "<invalid>"
	}
}

// AsString은 값을 문자열로 변환합니다.
func (v Value) AsString() string {
	switch v.Type {
	case TypeString:
		return v.StrVal
	case TypeNumber:
		if v.NumVal == math.Floor(v.NumVal) {
			return fmt.Sprintf("%.0f", v.NumVal)
		}
		return fmt.Sprintf("%g", v.NumVal)
	case TypeBool:
		return fmt.Sprintf("%t", v.BoolVal)
	case TypeUnknown:
		return "(known after apply)"
	case TypeNull:
		return ""
	default:
		return v.String()
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 평가 스코프 (Scope)
// ─────────────────────────────────────────────────────────────────────────────

// Scope는 표현식 평가에 사용되는 컨텍스트입니다.
// 실제: internal/lang/eval.go의 Scope
type Scope struct {
	Variables map[string]Value            // var.name → value
	Locals    map[string]Value            // local.name → value
	Resources map[string]map[string]Value // resource_type.name → attributes
	Data      map[string]map[string]Value // data.type.name → attributes
	Outputs   map[string]Value            // output.name → value
}

func NewScope() *Scope {
	return &Scope{
		Variables: make(map[string]Value),
		Locals:    make(map[string]Value),
		Resources: make(map[string]map[string]Value),
		Data:      make(map[string]map[string]Value),
		Outputs:   make(map[string]Value),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 내장 함수
// ─────────────────────────────────────────────────────────────────────────────

// BuiltinFunc는 내장 함수 시그니처입니다.
type BuiltinFunc func(args []Value) (Value, error)

// builtinFunctions는 Terraform 내장 함수 맵입니다.
// 실제: internal/lang/functions.go
var builtinFunctions = map[string]BuiltinFunc{
	"upper": funcUpper,
	"lower": funcLower,
	"length": funcLength,
	"format": funcFormat,
	"join":   funcJoin,
	"lookup": funcLookup,
	"element": funcElement,
	"tostring": funcToString,
	"tonumber": funcToNumber,
	"tobool":   funcToBool,
	"contains": funcContains,
	"concat":   funcConcat,
	"coalesce":  funcCoalesce,
}

func funcUpper(args []Value) (Value, error) {
	if len(args) != 1 {
		return NullVal(), fmt.Errorf("upper()는 인자 1개가 필요합니다")
	}
	if !args[0].IsKnown() {
		return UnknownVal(), nil
	}
	return StringVal(strings.ToUpper(args[0].AsString())), nil
}

func funcLower(args []Value) (Value, error) {
	if len(args) != 1 {
		return NullVal(), fmt.Errorf("lower()는 인자 1개가 필요합니다")
	}
	if !args[0].IsKnown() {
		return UnknownVal(), nil
	}
	return StringVal(strings.ToLower(args[0].AsString())), nil
}

func funcLength(args []Value) (Value, error) {
	if len(args) != 1 {
		return NullVal(), fmt.Errorf("length()는 인자 1개가 필요합니다")
	}
	if !args[0].IsKnown() {
		return UnknownVal(), nil
	}
	switch args[0].Type {
	case TypeString:
		return NumberVal(float64(len(args[0].StrVal))), nil
	case TypeList:
		return NumberVal(float64(len(args[0].ListVal))), nil
	case TypeMap:
		return NumberVal(float64(len(args[0].MapVal))), nil
	default:
		return NullVal(), fmt.Errorf("length()는 string, list, map 타입만 지원합니다")
	}
}

func funcFormat(args []Value) (Value, error) {
	if len(args) < 1 {
		return NullVal(), fmt.Errorf("format()는 최소 인자 1개가 필요합니다")
	}
	for _, a := range args {
		if !a.IsKnown() {
			return UnknownVal(), nil
		}
	}

	fmtStr := args[0].AsString()
	var fmtArgs []interface{}
	for _, a := range args[1:] {
		switch a.Type {
		case TypeString:
			fmtArgs = append(fmtArgs, a.StrVal)
		case TypeNumber:
			fmtArgs = append(fmtArgs, a.NumVal)
		case TypeBool:
			fmtArgs = append(fmtArgs, a.BoolVal)
		default:
			fmtArgs = append(fmtArgs, a.String())
		}
	}

	// %s, %d, %v 등 Go 포맷 지원
	result := fmt.Sprintf(fmtStr, fmtArgs...)
	return StringVal(result), nil
}

func funcJoin(args []Value) (Value, error) {
	if len(args) != 2 {
		return NullVal(), fmt.Errorf("join()는 인자 2개가 필요합니다 (구분자, 리스트)")
	}
	for _, a := range args {
		if !a.IsKnown() {
			return UnknownVal(), nil
		}
	}

	sep := args[0].AsString()
	if args[1].Type != TypeList {
		return NullVal(), fmt.Errorf("join()의 두 번째 인자는 list여야 합니다")
	}

	var parts []string
	for _, item := range args[1].ListVal {
		parts = append(parts, item.AsString())
	}

	return StringVal(strings.Join(parts, sep)), nil
}

func funcLookup(args []Value) (Value, error) {
	if len(args) < 2 || len(args) > 3 {
		return NullVal(), fmt.Errorf("lookup()는 인자 2~3개가 필요합니다 (맵, 키, [기본값])")
	}
	for _, a := range args {
		if !a.IsKnown() {
			return UnknownVal(), nil
		}
	}

	if args[0].Type != TypeMap {
		return NullVal(), fmt.Errorf("lookup()의 첫 번째 인자는 map이어야 합니다")
	}

	key := args[1].AsString()
	if val, ok := args[0].MapVal[key]; ok {
		return val, nil
	}

	if len(args) == 3 {
		return args[2], nil
	}

	return NullVal(), fmt.Errorf("키 '%s'을(를) 맵에서 찾을 수 없습니다", key)
}

func funcElement(args []Value) (Value, error) {
	if len(args) != 2 {
		return NullVal(), fmt.Errorf("element()는 인자 2개가 필요합니다 (리스트, 인덱스)")
	}
	for _, a := range args {
		if !a.IsKnown() {
			return UnknownVal(), nil
		}
	}

	if args[0].Type != TypeList {
		return NullVal(), fmt.Errorf("element()의 첫 번째 인자는 list여야 합니다")
	}

	idx := int(args[1].NumVal)
	list := args[0].ListVal

	if len(list) == 0 {
		return NullVal(), fmt.Errorf("빈 리스트에서 element()를 호출할 수 없습니다")
	}

	// Terraform의 element()는 모듈러 인덱싱 사용
	idx = idx % len(list)
	if idx < 0 {
		idx += len(list)
	}

	return list[idx], nil
}

func funcToString(args []Value) (Value, error) {
	if len(args) != 1 {
		return NullVal(), fmt.Errorf("tostring()는 인자 1개가 필요합니다")
	}
	if !args[0].IsKnown() {
		return UnknownVal(), nil
	}
	return StringVal(args[0].AsString()), nil
}

func funcToNumber(args []Value) (Value, error) {
	if len(args) != 1 {
		return NullVal(), fmt.Errorf("tonumber()는 인자 1개가 필요합니다")
	}
	if !args[0].IsKnown() {
		return UnknownVal(), nil
	}
	str := args[0].AsString()
	n, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return NullVal(), fmt.Errorf("'%s'을(를) number로 변환할 수 없습니다", str)
	}
	return NumberVal(n), nil
}

func funcToBool(args []Value) (Value, error) {
	if len(args) != 1 {
		return NullVal(), fmt.Errorf("tobool()는 인자 1개가 필요합니다")
	}
	if !args[0].IsKnown() {
		return UnknownVal(), nil
	}
	str := args[0].AsString()
	switch strings.ToLower(str) {
	case "true":
		return BoolVal(true), nil
	case "false":
		return BoolVal(false), nil
	default:
		return NullVal(), fmt.Errorf("'%s'을(를) bool로 변환할 수 없습니다", str)
	}
}

func funcContains(args []Value) (Value, error) {
	if len(args) != 2 {
		return NullVal(), fmt.Errorf("contains()는 인자 2개가 필요합니다 (리스트, 값)")
	}
	for _, a := range args {
		if !a.IsKnown() {
			return UnknownVal(), nil
		}
	}

	if args[0].Type != TypeList {
		return NullVal(), fmt.Errorf("contains()의 첫 번째 인자는 list여야 합니다")
	}

	searchStr := args[1].AsString()
	for _, item := range args[0].ListVal {
		if item.AsString() == searchStr {
			return BoolVal(true), nil
		}
	}
	return BoolVal(false), nil
}

func funcConcat(args []Value) (Value, error) {
	for _, a := range args {
		if !a.IsKnown() {
			return UnknownVal(), nil
		}
	}

	var result []Value
	for _, arg := range args {
		if arg.Type != TypeList {
			return NullVal(), fmt.Errorf("concat()의 모든 인자는 list여야 합니다")
		}
		result = append(result, arg.ListVal...)
	}
	return ListVal(result...), nil
}

func funcCoalesce(args []Value) (Value, error) {
	for _, a := range args {
		if !a.IsKnown() {
			return UnknownVal(), nil
		}
	}

	for _, arg := range args {
		if !arg.IsNull() && arg.AsString() != "" {
			return arg, nil
		}
	}
	return NullVal(), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 표현식 평가기
// ─────────────────────────────────────────────────────────────────────────────

// Evaluator는 Terraform 표현식을 평가합니다.
type Evaluator struct {
	scope     *Scope
	functions map[string]BuiltinFunc
}

func NewEvaluator(scope *Scope) *Evaluator {
	return &Evaluator{
		scope:     scope,
		functions: builtinFunctions,
	}
}

// Eval은 표현식 문자열을 평가합니다.
func (e *Evaluator) Eval(expr string) (Value, error) {
	expr = strings.TrimSpace(expr)

	// 1. 문자열 보간: "Hello, ${var.name}!"
	if strings.HasPrefix(expr, "\"") && strings.HasSuffix(expr, "\"") {
		return e.evalInterpolation(expr[1 : len(expr)-1])
	}

	// 2. 조건식: condition ? true_val : false_val
	if idx := strings.Index(expr, " ? "); idx >= 0 {
		return e.evalConditional(expr)
	}

	// 3. 함수 호출: func(args...)
	if parenIdx := strings.Index(expr, "("); parenIdx > 0 {
		funcName := strings.TrimSpace(expr[:parenIdx])
		if _, ok := e.functions[funcName]; ok {
			return e.evalFunctionCall(expr)
		}
	}

	// 4. 변수 참조: var.name
	if strings.HasPrefix(expr, "var.") {
		name := expr[4:]
		if val, ok := e.scope.Variables[name]; ok {
			return val, nil
		}
		return NullVal(), fmt.Errorf("변수 'var.%s'을(를) 찾을 수 없습니다", name)
	}

	// 5. 로컬 값 참조: local.name
	if strings.HasPrefix(expr, "local.") {
		name := expr[6:]
		if val, ok := e.scope.Locals[name]; ok {
			return val, nil
		}
		return NullVal(), fmt.Errorf("로컬 값 'local.%s'을(를) 찾을 수 없습니다", name)
	}

	// 6. 리소스 속성 참조: type.name.attr
	parts := strings.SplitN(expr, ".", 3)
	if len(parts) == 3 {
		// data 소스 확인
		if parts[0] == "data" {
			subParts := strings.SplitN(parts[1]+"."+parts[2], ".", 3)
			if len(subParts) >= 3 {
				key := subParts[0] + "." + subParts[1]
				if attrs, ok := e.scope.Data[key]; ok {
					if val, ok2 := attrs[subParts[2]]; ok2 {
						return val, nil
					}
				}
			}
		}

		// 일반 리소스
		resKey := parts[0] + "." + parts[1]
		if attrs, ok := e.scope.Resources[resKey]; ok {
			if val, ok2 := attrs[parts[2]]; ok2 {
				return val, nil
			}
			return NullVal(), fmt.Errorf("리소스 '%s'에 속성 '%s'이(가) 없습니다", resKey, parts[2])
		}
	}

	// 7. 리터럴 값
	return e.evalLiteral(expr)
}

// evalLiteral은 리터럴 값을 평가합니다.
func (e *Evaluator) evalLiteral(expr string) (Value, error) {
	// null
	if expr == "null" {
		return NullVal(), nil
	}

	// bool
	if expr == "true" {
		return BoolVal(true), nil
	}
	if expr == "false" {
		return BoolVal(false), nil
	}

	// number
	if n, err := strconv.ParseFloat(expr, 64); err == nil {
		return NumberVal(n), nil
	}

	// 따옴표 없는 문자열 (식별자)
	return StringVal(expr), nil
}

// evalInterpolation은 문자열 보간을 평가합니다.
// "Hello, ${var.name}! You have ${length(var.items)} items."
func (e *Evaluator) evalInterpolation(template string) (Value, error) {
	var result strings.Builder
	remaining := template

	for len(remaining) > 0 {
		// ${...} 찾기
		start := strings.Index(remaining, "${")
		if start < 0 {
			result.WriteString(remaining)
			break
		}

		// ${ 앞의 텍스트 추가
		result.WriteString(remaining[:start])
		remaining = remaining[start+2:]

		// 중괄호 매칭
		depth := 1
		end := -1
		for i := 0; i < len(remaining); i++ {
			if remaining[i] == '{' {
				depth++
			} else if remaining[i] == '}' {
				depth--
				if depth == 0 {
					end = i
					break
				}
			}
		}

		if end < 0 {
			return NullVal(), fmt.Errorf("닫히지 않은 보간 표현식")
		}

		// 내부 표현식 평가
		innerExpr := remaining[:end]
		val, err := e.Eval(innerExpr)
		if err != nil {
			return NullVal(), fmt.Errorf("보간 표현식 오류: %w", err)
		}

		// Unknown 값이면 전체 결과도 Unknown
		if !val.IsKnown() {
			return UnknownVal(), nil
		}

		result.WriteString(val.AsString())
		remaining = remaining[end+1:]
	}

	return StringVal(result.String()), nil
}

// evalConditional은 조건식을 평가합니다.
// condition ? true_val : false_val
func (e *Evaluator) evalConditional(expr string) (Value, error) {
	qIdx := strings.Index(expr, " ? ")
	if qIdx < 0 {
		return NullVal(), fmt.Errorf("유효하지 않은 조건식")
	}

	condExpr := strings.TrimSpace(expr[:qIdx])
	rest := expr[qIdx+3:]

	cIdx := strings.Index(rest, " : ")
	if cIdx < 0 {
		return NullVal(), fmt.Errorf("조건식에 ':' 구분자가 없습니다")
	}

	trueExpr := strings.TrimSpace(rest[:cIdx])
	falseExpr := strings.TrimSpace(rest[cIdx+3:])

	// 조건 평가
	condVal, err := e.Eval(condExpr)
	if err != nil {
		return NullVal(), fmt.Errorf("조건 평가 오류: %w", err)
	}

	// Unknown이면 결과도 Unknown
	if !condVal.IsKnown() {
		return UnknownVal(), nil
	}

	// 조건 판단
	isTruthy := false
	switch condVal.Type {
	case TypeBool:
		isTruthy = condVal.BoolVal
	case TypeString:
		isTruthy = condVal.StrVal != ""
	case TypeNumber:
		isTruthy = condVal.NumVal != 0
	case TypeNull:
		isTruthy = false
	default:
		isTruthy = true
	}

	if isTruthy {
		return e.Eval(trueExpr)
	}
	return e.Eval(falseExpr)
}

// evalFunctionCall은 함수 호출을 평가합니다.
func (e *Evaluator) evalFunctionCall(expr string) (Value, error) {
	parenIdx := strings.Index(expr, "(")
	funcName := strings.TrimSpace(expr[:parenIdx])

	// 마지막 ")" 찾기
	lastParen := strings.LastIndex(expr, ")")
	if lastParen < 0 {
		return NullVal(), fmt.Errorf("함수 호출에 닫는 괄호가 없습니다")
	}

	argsStr := expr[parenIdx+1 : lastParen]

	// 인자 파싱 (쉼표로 분리, 괄호 깊이 고려)
	args, err := e.parseArgs(argsStr)
	if err != nil {
		return NullVal(), fmt.Errorf("함수 인자 파싱 오류: %w", err)
	}

	// 인자 평가
	var evaledArgs []Value
	for _, argExpr := range args {
		val, err := e.Eval(argExpr)
		if err != nil {
			return NullVal(), fmt.Errorf("함수 인자 평가 오류: %w", err)
		}
		evaledArgs = append(evaledArgs, val)
	}

	// 함수 실행
	fn, ok := e.functions[funcName]
	if !ok {
		return NullVal(), fmt.Errorf("알 수 없는 함수: %s()", funcName)
	}

	return fn(evaledArgs)
}

// parseArgs는 함수 인자 문자열을 파싱합니다.
func (e *Evaluator) parseArgs(argsStr string) ([]string, error) {
	argsStr = strings.TrimSpace(argsStr)
	if argsStr == "" {
		return nil, nil
	}

	var args []string
	depth := 0
	inQuote := false
	start := 0

	for i := 0; i < len(argsStr); i++ {
		ch := argsStr[i]
		if ch == '"' && (i == 0 || argsStr[i-1] != '\\') {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				args = append(args, strings.TrimSpace(argsStr[start:i]))
				start = i + 1
			}
		}
	}
	args = append(args, strings.TrimSpace(argsStr[start:]))

	return args, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 헬퍼 함수
// ─────────────────────────────────────────────────────────────────────────────

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println()
}

func evalAndPrint(eval *Evaluator, expr string) {
	result, err := eval.Eval(expr)
	if err != nil {
		fmt.Printf("  %-50s → 오류: %v\n", expr, err)
	} else {
		fmt.Printf("  %-50s → %s (%s)\n", expr, result.String(), result.Type.String())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 메인 함수
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║          Terraform 표현식 평가(Expression Evaluation) 시뮬레이션    ║")
	fmt.Println("║                                                                      ║")
	fmt.Println("║  Value 타입 시스템, 함수, 조건식, 보간, Unknown 전파                ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	// 스코프 설정
	scope := NewScope()

	// 변수 설정
	scope.Variables["name"] = StringVal("terraform-user")
	scope.Variables["env"] = StringVal("production")
	scope.Variables["count"] = NumberVal(3)
	scope.Variables["enabled"] = BoolVal(true)
	scope.Variables["tags"] = MapVal(map[string]Value{
		"Environment": StringVal("production"),
		"Team":        StringVal("platform"),
		"Project":     StringVal("infra"),
	})
	scope.Variables["zones"] = ListVal(
		StringVal("us-east-1a"),
		StringVal("us-east-1b"),
		StringVal("us-east-1c"),
	)
	scope.Variables["instance_id"] = UnknownVal() // plan 단계에서 아직 모르는 값

	// 로컬 값 설정
	scope.Locals["prefix"] = StringVal("prod")
	scope.Locals["common_tags"] = MapVal(map[string]Value{
		"ManagedBy": StringVal("terraform"),
	})

	// 리소스 속성 설정
	scope.Resources["aws_vpc.main"] = map[string]Value{
		"id":         StringVal("vpc-abc123"),
		"cidr_block": StringVal("10.0.0.0/16"),
	}
	scope.Resources["aws_instance.web"] = map[string]Value{
		"id":            UnknownVal(), // plan 단계
		"ami":           StringVal("ami-12345"),
		"instance_type": StringVal("t2.micro"),
		"private_ip":    UnknownVal(),
	}
	scope.Resources["aws_subnet.public"] = map[string]Value{
		"id":                StringVal("subnet-pub123"),
		"availability_zone": StringVal("us-east-1a"),
	}

	eval := NewEvaluator(scope)

	// ─── 1. 변수 참조 ───
	printSeparator("1. 변수 참조 (var.xxx)")
	evalAndPrint(eval, "var.name")
	evalAndPrint(eval, "var.env")
	evalAndPrint(eval, "var.count")
	evalAndPrint(eval, "var.enabled")
	evalAndPrint(eval, "var.tags")
	evalAndPrint(eval, "var.zones")
	evalAndPrint(eval, "var.instance_id")
	evalAndPrint(eval, "var.nonexistent")

	// ─── 2. 로컬 값 참조 ───
	printSeparator("2. 로컬 값 참조 (local.xxx)")
	evalAndPrint(eval, "local.prefix")
	evalAndPrint(eval, "local.common_tags")

	// ─── 3. 리소스 속성 참조 ───
	printSeparator("3. 리소스 속성 참조 (type.name.attr)")
	evalAndPrint(eval, "aws_vpc.main.id")
	evalAndPrint(eval, "aws_vpc.main.cidr_block")
	evalAndPrint(eval, "aws_instance.web.ami")
	evalAndPrint(eval, "aws_instance.web.id")
	evalAndPrint(eval, "aws_instance.web.private_ip")
	evalAndPrint(eval, "aws_subnet.public.availability_zone")

	// ─── 4. 리터럴 값 ───
	printSeparator("4. 리터럴 값")
	evalAndPrint(eval, "true")
	evalAndPrint(eval, "false")
	evalAndPrint(eval, "null")
	evalAndPrint(eval, "42")
	evalAndPrint(eval, "3.14")

	// ─── 5. 내장 함수 ───
	printSeparator("5. 내장 함수")
	evalAndPrint(eval, "upper(var.name)")
	evalAndPrint(eval, "lower(var.env)")
	evalAndPrint(eval, "length(var.name)")
	evalAndPrint(eval, "length(var.zones)")
	evalAndPrint(eval, "length(var.tags)")

	fmt.Println()
	fmt.Println("  --- format() ---")
	evalAndPrint(eval, "format(\"%s-%s\", var.env, var.name)")

	fmt.Println()
	fmt.Println("  --- join() ---")
	evalAndPrint(eval, "join(\", \", var.zones)")

	fmt.Println()
	fmt.Println("  --- lookup() ---")
	evalAndPrint(eval, "lookup(var.tags, \"Team\", \"unknown\")")
	evalAndPrint(eval, "lookup(var.tags, \"Owner\", \"nobody\")")

	fmt.Println()
	fmt.Println("  --- element() ---")
	evalAndPrint(eval, "element(var.zones, 0)")
	evalAndPrint(eval, "element(var.zones, 1)")
	evalAndPrint(eval, "element(var.zones, 5)")

	fmt.Println()
	fmt.Println("  --- tostring() / tonumber() / tobool() ---")
	evalAndPrint(eval, "tostring(var.count)")
	evalAndPrint(eval, "tonumber(42)")
	evalAndPrint(eval, "tobool(true)")

	fmt.Println()
	fmt.Println("  --- contains() ---")
	evalAndPrint(eval, "contains(var.zones, \"us-east-1a\")")
	evalAndPrint(eval, "contains(var.zones, \"eu-west-1a\")")

	fmt.Println()
	fmt.Println("  --- coalesce() ---")
	evalAndPrint(eval, "coalesce(null, var.name)")

	// ─── 6. 조건식 ───
	printSeparator("6. 조건식 (condition ? true : false)")
	evalAndPrint(eval, "var.enabled ? var.env : \"disabled\"")
	evalAndPrint(eval, "false ? \"yes\" : \"no\"")
	evalAndPrint(eval, "var.count ? \"has_resources\" : \"empty\"")
	evalAndPrint(eval, "var.instance_id ? \"known\" : \"unknown\"")

	// ─── 7. 문자열 보간 ───
	printSeparator("7. 문자열 보간 (\"...${expr}...\")")
	evalAndPrint(eval, "\"Hello, ${var.name}!\"")
	evalAndPrint(eval, "\"${local.prefix}-${var.env}-web\"")
	evalAndPrint(eval, "\"VPC ID: ${aws_vpc.main.id}\"")
	evalAndPrint(eval, "\"AZ: ${aws_subnet.public.availability_zone}\"")
	evalAndPrint(eval, "\"Count: ${var.count} items\"")
	evalAndPrint(eval, "\"Instance: ${aws_instance.web.id}\"")

	// ─── 8. Unknown 값 전파 ───
	printSeparator("8. Unknown 값 전파")
	fmt.Println("  Unknown 값은 plan 단계에서 아직 결정되지 않은 값입니다.")
	fmt.Println("  Unknown이 포함된 모든 연산의 결과도 Unknown이 됩니다.")
	fmt.Println()

	evalAndPrint(eval, "var.instance_id")
	evalAndPrint(eval, "upper(var.instance_id)")
	evalAndPrint(eval, "length(var.instance_id)")
	evalAndPrint(eval, "\"prefix-${var.instance_id}\"")
	evalAndPrint(eval, "var.instance_id ? \"known\" : \"unknown\"")

	fmt.Println()
	fmt.Println("  리소스의 Unknown 속성:")
	evalAndPrint(eval, "aws_instance.web.id")
	evalAndPrint(eval, "aws_instance.web.private_ip")
	evalAndPrint(eval, "\"IP: ${aws_instance.web.private_ip}\"")

	// ─── 9. 오류 처리 ───
	printSeparator("9. 오류 처리")
	evalAndPrint(eval, "var.undefined_var")
	evalAndPrint(eval, "local.undefined_local")
	evalAndPrint(eval, "unknown_func(42)")
	evalAndPrint(eval, "upper()")
	evalAndPrint(eval, "length(true)")

	// ─── 아키텍처 요약 ───
	printSeparator("표현식 평가 아키텍처 요약")
	fmt.Print(`
  평가 흐름:

  HCL 표현식                Evaluator                  결과
  ┌───────────────┐    ┌─────────────────┐    ┌──────────────┐
  │ var.name      │───▶│ 변수 참조 해석   │───▶│ StringVal    │
  │ upper(var.x)  │───▶│ 함수 호출 평가   │───▶│ StringVal    │
  │ a ? b : c     │───▶│ 조건식 평가      │───▶│ Value        │
  │ "${var.x}"    │───▶│ 문자열 보간      │───▶│ StringVal    │
  │ aws.x.y.attr  │───▶│ 리소스 참조      │───▶│ Value/Unknown│
  └───────────────┘    └─────────────────┘    └──────────────┘
                              │
                       ┌──────▼──────┐
                       │   Scope     │
                       │ ┌─────────┐ │
                       │ │Variables│ │  var.name → "user"
                       │ │Locals   │ │  local.x  → "val"
                       │ │Resources│ │  aws_vpc.main.id → "vpc-xxx"
                       │ │Data     │ │  data.aws_ami.x.id → "ami-xxx"
                       │ └─────────┘ │
                       └─────────────┘

  Value 타입 시스템 (cty 기반):

    TypeString  → "hello"          문자열
    TypeNumber  → 42, 3.14         숫자
    TypeBool    → true, false      불리언
    TypeList    → ["a", "b"]       리스트
    TypeMap     → {key = "val"}    맵
    TypeNull    → null             널
    TypeUnknown → (known after apply)  미확정 값

  Unknown 전파 규칙:
    upper(unknown)     → unknown
    "${unknown}"       → unknown
    unknown ? a : b    → unknown
    → plan 단계에서 완전한 값을 알 수 없으므로 "(known after apply)" 표시`)
}
