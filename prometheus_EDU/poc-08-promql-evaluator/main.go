package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ============================================================================
// PromQL Evaluator PoC
// ============================================================================
// Prometheus의 promql/parser/lex.go, promql/parser/ast.go, promql/engine.go를
// 참고하여 PromQL 파서 및 평가기의 핵심 동작을 재현한다.
//
// 실제 Prometheus 구현:
//   - Lexer: promql/parser/lex.go — stateFn 기반 상태 머신 렉서
//   - AST:   promql/parser/ast.go — Node/Expr 인터페이스와 VectorSelector,
//            BinaryExpr, AggregateExpr, Call, NumberLiteral 등의 노드
//   - Engine: promql/engine.go — evaluator.eval()에서 switch로 AST 노드 타입별 평가
//   - Value: promql/value.go — Vector, Matrix, Scalar, Series 등 결과 타입
// ============================================================================

// ============================================================================
// 1. Labels — 시계열 식별자 (model/labels/labels.go 참조)
// ============================================================================

// Label은 단일 key=value 쌍이다.
type Label struct {
	Name  string
	Value string
}

// Labels는 정렬된 Label 슬라이스이다.
type Labels []Label

func (ls Labels) String() string {
	parts := make([]string, len(ls))
	for i, l := range ls {
		parts[i] = l.Name + "=" + strconv.Quote(l.Value)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// Get은 지정된 이름의 레이블 값을 반환한다.
func (ls Labels) Get(name string) string {
	for _, l := range ls {
		if l.Name == name {
			return l.Value
		}
	}
	return ""
}

// WithoutName은 __name__ 레이블을 제외한 레이블 집합을 반환한다.
func (ls Labels) WithoutName() Labels {
	result := make(Labels, 0, len(ls))
	for _, l := range ls {
		if l.Name != "__name__" {
			result = append(result, l)
		}
	}
	return result
}

// Hash는 레이블 집합의 해시값을 반환한다. 그룹핑 키로 사용한다.
func (ls Labels) Hash() string {
	return ls.String()
}

// SubsetByNames는 지정된 레이블 이름들만 포함하는 부분 집합을 반환한다.
func (ls Labels) SubsetByNames(names []string) Labels {
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}
	result := make(Labels, 0)
	for _, l := range ls {
		if nameSet[l.Name] {
			result = append(result, l)
		}
	}
	return result
}

// ExcludeNames는 지정된 레이블 이름들을 제외한 부분 집합을 반환한다.
func (ls Labels) ExcludeNames(names []string) Labels {
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}
	result := make(Labels, 0)
	for _, l := range ls {
		if !nameSet[l.Name] {
			result = append(result, l)
		}
	}
	return result
}

// ============================================================================
// 2. Sample & TimeSeries — 데이터 포인트와 시계열 (promql/value.go 참조)
// ============================================================================

// Sample은 레이블 + 타임스탬프 + 값을 가진 단일 데이터 포인트이다.
// Prometheus의 promql.Sample에 해당한다.
type Sample struct {
	Labels    Labels
	Timestamp int64   // 밀리초 단위
	Value     float64
}

// Vector는 동일 타임스탬프의 Sample 집합이다.
// Prometheus의 promql.Vector에 해당한다.
type Vector []Sample

// TimeSeries는 하나의 시계열에 대한 타임스탬프-값 쌍의 목록이다.
type TimeSeries struct {
	Labels Labels
	Points []TimePoint
}

// TimePoint은 타임스탬프-값 쌍이다.
type TimePoint struct {
	T int64   // 밀리초
	V float64
}

// ============================================================================
// 3. Storage — 사전 구성된 인메모리 시계열 저장소
// ============================================================================

// Storage는 시계열 데이터를 보유한다. Prometheus의 storage.Queryable에 해당한다.
type Storage struct {
	Series []TimeSeries
}

// Select는 레이블 매처와 일치하는 시계열을 반환한다.
// Prometheus의 storage.Querier.Select()에 해당한다.
func (s *Storage) Select(matchers []LabelMatcher) []TimeSeries {
	var result []TimeSeries
	for _, ts := range s.Series {
		if matchAll(ts.Labels, matchers) {
			result = append(result, ts)
		}
	}
	return result
}

// SelectRange는 지정된 시간 범위 내의 포인트만 포함하는 시계열을 반환한다.
func (s *Storage) SelectRange(matchers []LabelMatcher, minT, maxT int64) []TimeSeries {
	var result []TimeSeries
	for _, ts := range s.Series {
		if !matchAll(ts.Labels, matchers) {
			continue
		}
		var points []TimePoint
		for _, p := range ts.Points {
			if p.T >= minT && p.T <= maxT {
				points = append(points, p)
			}
		}
		if len(points) > 0 {
			result = append(result, TimeSeries{Labels: ts.Labels, Points: points})
		}
	}
	return result
}

func matchAll(labels Labels, matchers []LabelMatcher) bool {
	for _, m := range matchers {
		val := labels.Get(m.Name)
		switch m.Type {
		case MatchEqual:
			if val != m.Value {
				return false
			}
		case MatchNotEqual:
			if val == m.Value {
				return false
			}
		}
	}
	return true
}

// ============================================================================
// 4. Lexer — PromQL 토큰화 (promql/parser/lex.go 참조)
// ============================================================================
// 실제 Prometheus Lexer는 stateFn 기반의 상태 머신이다:
//   type stateFn func(*Lexer) stateFn
//   type Lexer struct { input string; state stateFn; pos Pos; ... }
// 여기서는 단순화된 버전을 구현하되, 핵심 토큰 타입은 실제와 동일하게 유지한다.

// TokenType은 토큰의 종류를 나타낸다.
// 실제 parser/lex.go의 ItemType에 해당한다.
type TokenType int

const (
	// 기본 토큰
	TokenEOF        TokenType = iota
	TokenError
	TokenIdentifier         // 메트릭 이름, 레이블 이름 등
	TokenNumber             // 숫자 리터럴
	TokenString             // 문자열 리터럴 (따옴표 포함)
	TokenDuration           // 5m, 1h 등

	// 연산자 — parser/lex.go의 operatorsStart ~ operatorsEnd 사이 정의
	TokenAdd        // +
	TokenSub        // -
	TokenMul        // *
	TokenDiv        // /
	TokenGtr        // >
	TokenLss        // <
	TokenEqlC       // ==

	// 구분자
	TokenLeftParen  // (
	TokenRightParen // )
	TokenLeftBrace  // {
	TokenRightBrace // }
	TokenLeftBracket  // [
	TokenRightBracket // ]
	TokenComma      // ,
	TokenEql        // = (레이블 매처)

	// 키워드 — parser/lex.go의 key 맵에 정의
	TokenBy
	TokenWithout
	TokenSum
	TokenAvg
	TokenCount
	TokenMax
	TokenMin

	// 함수
	TokenRate
)

// Token은 렉서가 생성하는 토큰이다. parser/lex.go의 Item에 해당한다.
type Token struct {
	Type  TokenType
	Value string
	Pos   int
}

func (t Token) String() string {
	switch t.Type {
	case TokenEOF:
		return "EOF"
	case TokenError:
		return fmt.Sprintf("ERROR(%s)", t.Value)
	default:
		return fmt.Sprintf("%q", t.Value)
	}
}

var keywords = map[string]TokenType{
	"by":      TokenBy,
	"without": TokenWithout,
	"sum":     TokenSum,
	"avg":     TokenAvg,
	"count":   TokenCount,
	"max":     TokenMax,
	"min":     TokenMin,
	"rate":    TokenRate,
}

// Lexer는 PromQL 문자열을 토큰으로 분해한다.
// 실제 Prometheus의 Lexer는 stateFn 패턴을 사용하지만,
// 여기서는 간소화된 루프 기반 구현을 사용한다.
type Lexer struct {
	input  string
	pos    int
	tokens []Token
}

// Lex는 입력 문자열을 토큰화한다.
func Lex(input string) []Token {
	l := &Lexer{input: input}
	l.tokenize()
	return l.tokens
}

func (l *Lexer) tokenize() {
	for l.pos < len(l.input) {
		l.skipSpaces()
		if l.pos >= len(l.input) {
			break
		}

		ch := l.input[l.pos]

		switch {
		case ch == '(':
			l.emit(TokenLeftParen, "(")
		case ch == ')':
			l.emit(TokenRightParen, ")")
		case ch == '{':
			l.emit(TokenLeftBrace, "{")
		case ch == '}':
			l.emit(TokenRightBrace, "}")
		case ch == '[':
			l.emit(TokenLeftBracket, "[")
		case ch == ']':
			l.emit(TokenRightBracket, "]")
		case ch == ',':
			l.emit(TokenComma, ",")
		case ch == '+':
			l.emit(TokenAdd, "+")
		case ch == '-':
			l.emit(TokenSub, "-")
		case ch == '*':
			l.emit(TokenMul, "*")
		case ch == '/':
			l.emit(TokenDiv, "/")
		case ch == '>':
			l.emit(TokenGtr, ">")
		case ch == '<':
			l.emit(TokenLss, "<")
		case ch == '=' && l.pos+1 < len(l.input) && l.input[l.pos+1] == '=':
			l.tokens = append(l.tokens, Token{Type: TokenEqlC, Value: "==", Pos: l.pos})
			l.pos += 2
		case ch == '=':
			l.emit(TokenEql, "=")
		case ch == '"':
			l.lexString()
		case isDigit(rune(ch)) || (ch == '.' && l.pos+1 < len(l.input) && isDigit(rune(l.input[l.pos+1]))):
			l.lexNumber()
		case isAlpha(rune(ch)):
			l.lexIdentifierOrKeyword()
		default:
			l.tokens = append(l.tokens, Token{Type: TokenError, Value: string(ch), Pos: l.pos})
			l.pos++
		}
	}
	l.tokens = append(l.tokens, Token{Type: TokenEOF, Pos: l.pos})
}

func (l *Lexer) emit(typ TokenType, val string) {
	l.tokens = append(l.tokens, Token{Type: typ, Value: val, Pos: l.pos})
	l.pos++
}

func (l *Lexer) skipSpaces() {
	for l.pos < len(l.input) && (l.input[l.pos] == ' ' || l.input[l.pos] == '\t' || l.input[l.pos] == '\n') {
		l.pos++
	}
}

func (l *Lexer) lexString() {
	start := l.pos
	l.pos++ // skip opening quote
	for l.pos < len(l.input) && l.input[l.pos] != '"' {
		if l.input[l.pos] == '\\' {
			l.pos++ // skip escaped char
		}
		l.pos++
	}
	if l.pos < len(l.input) {
		l.pos++ // skip closing quote
	}
	// 따옴표 내부 문자열만 추출
	val := l.input[start+1 : l.pos-1]
	l.tokens = append(l.tokens, Token{Type: TokenString, Value: val, Pos: start})
}

func (l *Lexer) lexNumber() {
	start := l.pos
	for l.pos < len(l.input) && (isDigit(rune(l.input[l.pos])) || l.input[l.pos] == '.') {
		l.pos++
	}
	// Duration 감지: 숫자 뒤에 s/m/h/d/w/y 단위가 오면 Duration 토큰으로 처리
	// 실제 lexer의 lexNumberOrDuration과 동일한 로직
	if l.pos < len(l.input) && strings.ContainsRune("smhdwy", rune(l.input[l.pos])) {
		l.pos++ // 단위 문자 포함
		// ms 같은 복합 단위 지원
		if l.pos < len(l.input) && l.input[l.pos] == 's' {
			l.pos++
		}
		l.tokens = append(l.tokens, Token{Type: TokenDuration, Value: l.input[start:l.pos], Pos: start})
		return
	}
	l.tokens = append(l.tokens, Token{Type: TokenNumber, Value: l.input[start:l.pos], Pos: start})
}

func (l *Lexer) lexIdentifierOrKeyword() {
	start := l.pos
	for l.pos < len(l.input) && (isAlphaNumeric(rune(l.input[l.pos])) || l.input[l.pos] == '_' || l.input[l.pos] == ':') {
		l.pos++
	}
	word := l.input[start:l.pos]

	// 숫자+단위 형태 (예: 5m)인지 검사 → Duration
	if l.pos < len(l.input) || len(word) > 0 {
		// "[5m]" 형태의 기간 표현 확인
	}

	// 키워드 확인 — 실제 parser/lex.go의 key 맵 참조
	if typ, ok := keywords[strings.ToLower(word)]; ok {
		l.tokens = append(l.tokens, Token{Type: typ, Value: word, Pos: start})
		return
	}

	l.tokens = append(l.tokens, Token{Type: TokenIdentifier, Value: word, Pos: start})
}

func isAlpha(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isDigit(r rune) bool {
	return r >= '0' && r <= '9'
}

func isAlphaNumeric(r rune) bool {
	return isAlpha(r) || isDigit(r)
}

// ============================================================================
// 5. AST — 추상 구문 트리 (promql/parser/ast.go 참조)
// ============================================================================
// 실제 Prometheus AST 노드:
//   - VectorSelector: Name, LabelMatchers []*labels.Matcher
//   - BinaryExpr: Op ItemType, LHS/RHS Expr, VectorMatching
//   - AggregateExpr: Op ItemType, Expr Expr, Grouping []string, Without bool
//   - Call: Func *Function, Args Expressions
//   - NumberLiteral: Val float64
//   - MatrixSelector: VectorSelector Expr, Range time.Duration

// Expr은 모든 AST 노드의 인터페이스이다.
type Expr interface {
	String() string
	exprNode()
}

// MatchType은 레이블 매처의 비교 방식이다.
type MatchType int

const (
	MatchEqual    MatchType = iota // =
	MatchNotEqual                  // !=
)

// LabelMatcher는 레이블 매칭 조건이다. model/labels/matcher.go의 Matcher에 해당한다.
type LabelMatcher struct {
	Type  MatchType
	Name  string
	Value string
}

func (m LabelMatcher) String() string {
	op := "="
	if m.Type == MatchNotEqual {
		op = "!="
	}
	return fmt.Sprintf("%s%s%q", m.Name, op, m.Value)
}

// VectorSelector는 벡터 셀렉터 노드이다.
// 실제: parser/ast.go의 VectorSelector{Name, LabelMatchers, Series, Offset, ...}
type VectorSelector struct {
	Name     string
	Matchers []LabelMatcher
}

func (v *VectorSelector) String() string {
	if len(v.Matchers) == 0 {
		return v.Name
	}
	parts := make([]string, len(v.Matchers))
	for i, m := range v.Matchers {
		parts[i] = m.String()
	}
	return v.Name + "{" + strings.Join(parts, ", ") + "}"
}
func (*VectorSelector) exprNode() {}

// NumberLiteral은 숫자 상수 노드이다.
// 실제: parser/ast.go의 NumberLiteral{Val float64}
type NumberLiteral struct {
	Val float64
}

func (n *NumberLiteral) String() string {
	return strconv.FormatFloat(n.Val, 'f', -1, 64)
}
func (*NumberLiteral) exprNode() {}

// BinaryExpr은 이항 연산 노드이다.
// 실제: parser/ast.go의 BinaryExpr{Op ItemType, LHS/RHS Expr, VectorMatching}
type BinaryExpr struct {
	Op  string // "+", "-", "*", "/", ">", "<", "=="
	LHS Expr
	RHS Expr
}

func (b *BinaryExpr) String() string {
	return fmt.Sprintf("(%s %s %s)", b.LHS.String(), b.Op, b.RHS.String())
}
func (*BinaryExpr) exprNode() {}

// AggregateExpr은 집계 연산 노드이다.
// 실제: parser/ast.go의 AggregateExpr{Op, Expr, Grouping []string, Without bool}
type AggregateExpr struct {
	Op       string   // "sum", "avg", "count", "max", "min"
	Expr     Expr
	Grouping []string
	Without  bool
}

func (a *AggregateExpr) String() string {
	clause := "by"
	if a.Without {
		clause = "without"
	}
	if len(a.Grouping) > 0 {
		return fmt.Sprintf("%s %s (%s) (%s)", a.Op, clause, strings.Join(a.Grouping, ", "), a.Expr.String())
	}
	return fmt.Sprintf("%s(%s)", a.Op, a.Expr.String())
}
func (*AggregateExpr) exprNode() {}

// FunctionCall은 함수 호출 노드이다.
// 실제: parser/ast.go의 Call{Func *Function, Args Expressions}
type FunctionCall struct {
	Name string // "rate"
	Args []Expr
}

func (f *FunctionCall) String() string {
	argStrs := make([]string, len(f.Args))
	for i, a := range f.Args {
		argStrs[i] = a.String()
	}
	return fmt.Sprintf("%s(%s)", f.Name, strings.Join(argStrs, ", "))
}
func (*FunctionCall) exprNode() {}

// MatrixSelector는 범위 벡터 셀렉터이다.
// 실제: parser/ast.go의 MatrixSelector{VectorSelector Expr, Range time.Duration}
type MatrixSelector struct {
	VectorSelector *VectorSelector
	Range          time.Duration
}

func (m *MatrixSelector) String() string {
	return fmt.Sprintf("%s[%s]", m.VectorSelector.String(), m.Range)
}
func (*MatrixSelector) exprNode() {}

// ============================================================================
// 6. Parser — 재귀 하강 파서
// ============================================================================
// 실제 Prometheus는 yacc 생성 파서(generated_parser.y.go)를 사용하지만,
// 핵심 구조는 토큰을 읽어 AST 노드를 생성하는 것이다.
// 여기서는 재귀 하강 파서로 핵심 PromQL 문법을 처리한다.

// Parser는 토큰 스트림에서 AST를 구축한다.
type Parser struct {
	tokens []Token
	pos    int
}

func NewParser(tokens []Token) *Parser {
	return &Parser{tokens: tokens}
}

func (p *Parser) peek() Token {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return Token{Type: TokenEOF}
}

func (p *Parser) next() Token {
	t := p.peek()
	p.pos++
	return t
}

func (p *Parser) expect(typ TokenType) Token {
	t := p.next()
	if t.Type != typ {
		panic(fmt.Sprintf("expected token type %d, got %d (%q)", typ, t.Type, t.Value))
	}
	return t
}

// Parse는 전체 표현식을 파싱한다.
func (p *Parser) Parse() Expr {
	return p.parseExpr()
}

// parseExpr은 이항 연산자를 처리한다.
// 실제 Prometheus 파서에서도 precedence climbing으로 연산자 우선순위를 처리한다.
func (p *Parser) parseExpr() Expr {
	left := p.parsePrimary()

	for {
		t := p.peek()
		var op string
		switch t.Type {
		case TokenAdd:
			op = "+"
		case TokenSub:
			op = "-"
		case TokenMul:
			op = "*"
		case TokenDiv:
			op = "/"
		case TokenGtr:
			op = ">"
		case TokenLss:
			op = "<"
		case TokenEqlC:
			op = "=="
		default:
			return left
		}
		p.next() // consume operator
		right := p.parsePrimary()
		left = &BinaryExpr{Op: op, LHS: left, RHS: right}
	}
}

// parsePrimary는 기본 표현식(셀렉터, 숫자, 집계, 함수, 괄호)을 처리한다.
func (p *Parser) parsePrimary() Expr {
	t := p.peek()

	switch t.Type {
	case TokenNumber:
		p.next()
		val, _ := strconv.ParseFloat(t.Value, 64)
		return &NumberLiteral{Val: val}

	case TokenLeftParen:
		p.next() // (
		expr := p.parseExpr()
		p.expect(TokenRightParen) // )
		return expr

	case TokenSum, TokenAvg, TokenCount, TokenMax, TokenMin:
		return p.parseAggregate()

	case TokenRate:
		return p.parseFunctionCall()

	case TokenIdentifier:
		return p.parseVectorSelectorOrMatrix()

	default:
		panic(fmt.Sprintf("unexpected token: %v", t))
	}
}

// parseAggregate는 집계 표현식을 파싱한다.
// 문법: sum [by|without (label, ...)] (expr)
func (p *Parser) parseAggregate() Expr {
	opToken := p.next()
	op := strings.ToLower(opToken.Value)

	var grouping []string
	without := false

	// by/without 절 확인
	if p.peek().Type == TokenBy || p.peek().Type == TokenWithout {
		if p.peek().Type == TokenWithout {
			without = true
		}
		p.next() // consume by/without

		p.expect(TokenLeftParen)
		for p.peek().Type != TokenRightParen {
			t := p.expect(TokenIdentifier)
			grouping = append(grouping, t.Value)
			if p.peek().Type == TokenComma {
				p.next()
			}
		}
		p.expect(TokenRightParen)
	}

	// 표현식
	p.expect(TokenLeftParen)
	expr := p.parseExpr()
	p.expect(TokenRightParen)

	// grouping 정렬 — 실제 engine.go의 eval에서도 slices.Sort(sortedGrouping) 수행
	sort.Strings(grouping)

	return &AggregateExpr{
		Op:       op,
		Expr:     expr,
		Grouping: grouping,
		Without:  without,
	}
}

// parseFunctionCall은 함수 호출을 파싱한다.
// 현재 rate()만 지원한다.
func (p *Parser) parseFunctionCall() Expr {
	nameToken := p.next()
	name := nameToken.Value

	p.expect(TokenLeftParen)
	var args []Expr
	for p.peek().Type != TokenRightParen {
		args = append(args, p.parseExpr())
		if p.peek().Type == TokenComma {
			p.next()
		}
	}
	p.expect(TokenRightParen)

	return &FunctionCall{Name: name, Args: args}
}

// parseVectorSelectorOrMatrix는 벡터 셀렉터 또는 매트릭스 셀렉터를 파싱한다.
func (p *Parser) parseVectorSelectorOrMatrix() Expr {
	nameToken := p.next()
	name := nameToken.Value

	var matchers []LabelMatcher
	// __name__ 매처 자동 추가 — 실제 파서도 메트릭 이름을 __name__="..." 매처로 변환
	matchers = append(matchers, LabelMatcher{Type: MatchEqual, Name: "__name__", Value: name})

	// 레이블 매처 파싱: {label="value", ...}
	if p.peek().Type == TokenLeftBrace {
		p.next() // {
		for p.peek().Type != TokenRightBrace {
			labelName := p.expect(TokenIdentifier)
			p.expect(TokenEql) // =
			labelVal := p.expect(TokenString)
			matchers = append(matchers, LabelMatcher{
				Type:  MatchEqual,
				Name:  labelName.Value,
				Value: labelVal.Value,
			})
			if p.peek().Type == TokenComma {
				p.next()
			}
		}
		p.expect(TokenRightBrace)
	}

	vs := &VectorSelector{Name: name, Matchers: matchers}

	// 범위 벡터 [5m] 확인
	if p.peek().Type == TokenLeftBracket {
		p.next() // [
		durToken := p.next()
		if durToken.Type != TokenDuration {
			panic(fmt.Sprintf("expected duration inside brackets, got %v", durToken))
		}
		dur := parseDuration(durToken.Value)
		p.expect(TokenRightBracket) // ]
		return &MatrixSelector{VectorSelector: vs, Range: dur}
	}

	return vs
}

// parseDuration은 "5m", "1h" 등의 기간 문자열을 파싱한다.
func parseDuration(s string) time.Duration {
	// 간단한 구현: 숫자+단위
	if len(s) < 2 {
		return 0
	}
	numStr := s[:len(s)-1]
	unit := s[len(s)-1]
	num, err := strconv.Atoi(numStr)
	if err != nil {
		return 0
	}
	switch unit {
	case 's':
		return time.Duration(num) * time.Second
	case 'm':
		return time.Duration(num) * time.Minute
	case 'h':
		return time.Duration(num) * time.Hour
	case 'd':
		return time.Duration(num) * 24 * time.Hour
	default:
		return 0
	}
}

// ============================================================================
// 7. AST 시각화
// ============================================================================

// PrintAST는 AST를 트리 형태로 출력한다.
func PrintAST(expr Expr, indent string, isLast bool) {
	prefix := indent
	if isLast {
		prefix += "└── "
	} else {
		prefix += "├── "
	}

	childIndent := indent
	if isLast {
		childIndent += "    "
	} else {
		childIndent += "│   "
	}

	switch e := expr.(type) {
	case *VectorSelector:
		matcherStrs := make([]string, len(e.Matchers))
		for i, m := range e.Matchers {
			matcherStrs[i] = m.String()
		}
		fmt.Printf("%sVectorSelector: %s {%s}\n", prefix, e.Name, strings.Join(matcherStrs, ", "))

	case *NumberLiteral:
		fmt.Printf("%sNumberLiteral: %v\n", prefix, e.Val)

	case *BinaryExpr:
		fmt.Printf("%sBinaryExpr: op=%s\n", prefix, e.Op)
		PrintAST(e.LHS, childIndent, false)
		PrintAST(e.RHS, childIndent, true)

	case *AggregateExpr:
		clause := "by"
		if e.Without {
			clause = "without"
		}
		groupStr := ""
		if len(e.Grouping) > 0 {
			groupStr = fmt.Sprintf(" %s (%s)", clause, strings.Join(e.Grouping, ", "))
		}
		fmt.Printf("%sAggregateExpr: %s%s\n", prefix, e.Op, groupStr)
		PrintAST(e.Expr, childIndent, true)

	case *FunctionCall:
		fmt.Printf("%sFunctionCall: %s\n", prefix, e.Name)
		for i, arg := range e.Args {
			PrintAST(arg, childIndent, i == len(e.Args)-1)
		}

	case *MatrixSelector:
		fmt.Printf("%sMatrixSelector: range=%s\n", prefix, e.Range)
		PrintAST(e.VectorSelector, childIndent, true)
	}
}

// ============================================================================
// 8. Evaluator — AST 평가기 (promql/engine.go 참조)
// ============================================================================
// 실제 Prometheus의 evaluator.eval()은 큰 switch 문으로 AST 노드 타입별 처리:
//   case *parser.AggregateExpr:  → ev.rangeEvalAgg(...)
//   case *parser.Call:           → FunctionCalls[e.Func.Name](...)
//   case *parser.BinaryExpr:    → ev.evalBinary(...)
//   case *parser.VectorSelector: → ev.evalSeries(...)
//   case *parser.NumberLiteral:  → Scalar{V: e.Val}
//
// 이 PoC는 instant query (단일 시점) 평가만 구현한다.

// Evaluator는 AST를 평가하여 Vector를 반환한다.
type Evaluator struct {
	Storage   *Storage
	Timestamp int64 // 평가 시점 (밀리초)
	// lookbackDelta: 실제 Prometheus에서는 기본 5분.
	// VectorSelector가 정확한 타임스탬프에 데이터가 없으면
	// lookbackDelta 이내의 가장 최근 데이터를 반환한다.
	LookbackDelta int64 // 밀리초 단위
}

// Eval은 AST 노드를 평가하여 Vector를 반환한다.
// 실제 engine.go의 evaluator.eval(ctx, expr)에 해당한다.
func (ev *Evaluator) Eval(expr Expr) Vector {
	switch e := expr.(type) {

	case *VectorSelector:
		return ev.evalVectorSelector(e)

	case *NumberLiteral:
		// 실제: Scalar{T: ev.startTimestamp, V: e.Val}
		return Vector{{
			Labels:    Labels{},
			Timestamp: ev.Timestamp,
			Value:     e.Val,
		}}

	case *BinaryExpr:
		return ev.evalBinaryExpr(e)

	case *AggregateExpr:
		return ev.evalAggregateExpr(e)

	case *FunctionCall:
		return ev.evalFunctionCall(e)

	case *MatrixSelector:
		// MatrixSelector는 단독으로 평가되지 않고, rate() 등의 함수 내에서 사용된다.
		// 여기서는 가장 최근 값을 반환
		return ev.evalVectorSelector(e.VectorSelector)

	default:
		panic(fmt.Sprintf("unknown expression type: %T", expr))
	}
}

// evalVectorSelector는 벡터 셀렉터를 평가한다.
// 실제 engine.go에서:
//   1. checkAndExpandSeriesSet() → storage에서 매칭 시계열 조회
//   2. ev.evalSeries() → 각 시계열에서 lookbackDelta 내 가장 최근 샘플 선택
func (ev *Evaluator) evalVectorSelector(vs *VectorSelector) Vector {
	series := ev.Storage.Select(vs.Matchers)
	var result Vector

	for _, ts := range series {
		// lookbackDelta 내에서 가장 최근 포인트를 찾는다.
		// 실제 engine.go의 evaluator.evalSeries()에서:
		//   it := storage.NewMemoizedEmptyIterator(durationMilliseconds(ev.lookbackDelta))
		//   → Seek(refTime) → At() 으로 가장 가까운 샘플 선택
		minT := ev.Timestamp - ev.LookbackDelta
		var bestPoint *TimePoint
		for i := range ts.Points {
			p := &ts.Points[i]
			if p.T > ev.Timestamp {
				continue
			}
			if p.T < minT {
				continue
			}
			if bestPoint == nil || p.T > bestPoint.T {
				bestPoint = p
			}
		}
		if bestPoint != nil {
			result = append(result, Sample{
				Labels:    ts.Labels,
				Timestamp: ev.Timestamp,
				Value:     bestPoint.V,
			})
		}
	}
	return result
}

// evalBinaryExpr은 이항 연산을 평가한다.
// 실제 engine.go에서 BinaryExpr 평가 흐름:
//   - 양쪽이 모두 Scalar이면 → 단순 연산
//   - Vector vs Scalar → 각 샘플에 연산 적용
//   - Vector vs Vector → VectorMatching(on/ignoring)으로 레이블 매칭 후 연산
func (ev *Evaluator) evalBinaryExpr(b *BinaryExpr) Vector {
	lhs := ev.Eval(b.LHS)
	rhs := ev.Eval(b.RHS)

	// Case 1: 오른쪽이 스칼라(단일 값, 레이블 없음)인 경우 → Vector vs Scalar
	if len(rhs) == 1 && len(rhs[0].Labels) == 0 {
		return ev.binaryOpVectorScalar(b.Op, lhs, rhs[0].Value)
	}

	// Case 2: 왼쪽이 스칼라인 경우 → Scalar vs Vector
	if len(lhs) == 1 && len(lhs[0].Labels) == 0 {
		return ev.binaryOpScalarVector(b.Op, lhs[0].Value, rhs)
	}

	// Case 3: Vector vs Vector — 레이블 매칭 (one-to-one)
	// 실제 engine.go에서는 VectorMatching.MatchingLabels로 그룹핑 키를 만들어 매칭
	return ev.binaryOpVectorVector(b.Op, lhs, rhs)
}

func (ev *Evaluator) binaryOpVectorScalar(op string, vec Vector, scalar float64) Vector {
	var result Vector
	for _, s := range vec {
		val, keep := applyBinaryOp(op, s.Value, scalar)
		if keep {
			result = append(result, Sample{
				Labels:    s.Labels,
				Timestamp: ev.Timestamp,
				Value:     val,
			})
		}
	}
	return result
}

func (ev *Evaluator) binaryOpScalarVector(op string, scalar float64, vec Vector) Vector {
	var result Vector
	for _, s := range vec {
		val, keep := applyBinaryOp(op, scalar, s.Value)
		if keep {
			result = append(result, Sample{
				Labels:    s.Labels,
				Timestamp: ev.Timestamp,
				Value:     val,
			})
		}
	}
	return result
}

func (ev *Evaluator) binaryOpVectorVector(op string, lhs, rhs Vector) Vector {
	// __name__을 제외한 레이블로 매칭 (one-to-one)
	rhsMap := make(map[string]Sample)
	for _, s := range rhs {
		key := s.Labels.WithoutName().Hash()
		rhsMap[key] = s
	}

	var result Vector
	for _, ls := range lhs {
		key := ls.Labels.WithoutName().Hash()
		if rs, ok := rhsMap[key]; ok {
			val, keep := applyBinaryOp(op, ls.Value, rs.Value)
			if keep {
				// 이항 연산 결과에서 __name__은 제거한다 (실제 Prometheus 동작)
				result = append(result, Sample{
					Labels:    ls.Labels.WithoutName(),
					Timestamp: ev.Timestamp,
					Value:     val,
				})
			}
		}
	}
	return result
}

// applyBinaryOp는 이항 연산을 적용한다.
// 비교 연산자는 필터링 역할: 조건이 false이면 keep=false.
func applyBinaryOp(op string, a, b float64) (float64, bool) {
	switch op {
	case "+":
		return a + b, true
	case "-":
		return a - b, true
	case "*":
		return a * b, true
	case "/":
		if b == 0 {
			return math.NaN(), true
		}
		return a / b, true
	case ">":
		// 비교 연산자는 필터: 조건 만족 시에만 원래 값 유지
		return a, a > b
	case "<":
		return a, a < b
	case "==":
		return a, a == b
	default:
		return 0, false
	}
}

// evalAggregateExpr은 집계 연산을 평가한다.
// 실제 engine.go의 evaluator.rangeEvalAgg() → aggregation() 함수:
//   1. generateGroupingKey(metric, grouping, without)로 그룹 키 계산
//   2. 그룹별로 샘플 수집
//   3. op에 따라 sum, avg, count, max, min 등 계산
func (ev *Evaluator) evalAggregateExpr(a *AggregateExpr) Vector {
	inner := ev.Eval(a.Expr)

	// 그룹핑: 각 샘플을 그룹 키로 분류
	type group struct {
		labels  Labels
		samples []float64
	}
	groups := make(map[string]*group)

	for _, s := range inner {
		var groupLabels Labels
		if a.Without {
			// without: 지정 레이블 제외 + __name__ 제외
			excludeSet := make(map[string]bool, len(a.Grouping)+1)
			excludeSet["__name__"] = true
			for _, g := range a.Grouping {
				excludeSet[g] = true
			}
			groupLabels = s.Labels.ExcludeNames(a.Grouping)
			groupLabels = groupLabels.WithoutName()
			_ = excludeSet
		} else {
			// by: 지정 레이블만 포함
			groupLabels = s.Labels.SubsetByNames(a.Grouping)
		}

		key := groupLabels.Hash()
		g, ok := groups[key]
		if !ok {
			g = &group{labels: groupLabels}
			groups[key] = g
		}
		g.samples = append(g.samples, s.Value)
	}

	// 집계 연산 적용
	var result Vector
	for _, g := range groups {
		var val float64
		switch a.Op {
		case "sum":
			for _, v := range g.samples {
				val += v
			}
		case "avg":
			for _, v := range g.samples {
				val += v
			}
			val /= float64(len(g.samples))
		case "count":
			val = float64(len(g.samples))
		case "max":
			val = g.samples[0]
			for _, v := range g.samples[1:] {
				if v > val {
					val = v
				}
			}
		case "min":
			val = g.samples[0]
			for _, v := range g.samples[1:] {
				if v < val {
					val = v
				}
			}
		}

		result = append(result, Sample{
			Labels:    g.labels,
			Timestamp: ev.Timestamp,
			Value:     val,
		})
	}

	// 결과 정렬 (결정적 출력을 위해)
	sort.Slice(result, func(i, j int) bool {
		return result[i].Labels.Hash() < result[j].Labels.Hash()
	})

	return result
}

// evalFunctionCall은 함수 호출을 평가한다.
// 현재 rate()만 구현한다.
//
// rate() 실제 구현 (promql/functions.go):
//   - funcRate → extrapolatedRate(vals, args, enh, true, true)
//   - extrapolatedRate():
//     1. 범위 내 첫 번째/마지막 샘플 찾기
//     2. resultValue = lastValue - firstValue (counter 가정)
//     3. 외삽(extrapolation) 적용
//     4. resultValue / rangeDuration 반환
//
// 이 PoC는 간소화된 rate: (last - first) / range_seconds
func (ev *Evaluator) evalFunctionCall(f *FunctionCall) Vector {
	switch f.Name {
	case "rate":
		if len(f.Args) != 1 {
			panic("rate() requires exactly 1 argument")
		}
		ms, ok := f.Args[0].(*MatrixSelector)
		if !ok {
			panic("rate() requires a matrix selector argument")
		}
		return ev.evalRate(ms)
	default:
		panic(fmt.Sprintf("unknown function: %s", f.Name))
	}
}

// evalRate는 rate() 함수를 평가한다.
// rate = (last - first) / range_seconds
func (ev *Evaluator) evalRate(ms *MatrixSelector) Vector {
	rangeDur := ms.Range.Milliseconds()
	minT := ev.Timestamp - rangeDur
	maxT := ev.Timestamp

	series := ev.Storage.SelectRange(ms.VectorSelector.Matchers, minT, maxT)
	var result Vector

	for _, ts := range series {
		if len(ts.Points) < 2 {
			continue
		}

		// 실제 extrapolatedRate에서는 외삽(extrapolation)을 수행하지만,
		// 여기서는 단순 (last-first)/duration으로 간소화
		first := ts.Points[0]
		last := ts.Points[len(ts.Points)-1]

		rangeSeconds := float64(last.T-first.T) / 1000.0
		if rangeSeconds <= 0 {
			continue
		}

		rate := (last.V - first.V) / rangeSeconds

		// rate 결과에서 __name__ 레이블은 제거 (실제 Prometheus 동작)
		result = append(result, Sample{
			Labels:    ts.Labels.WithoutName(),
			Timestamp: ev.Timestamp,
			Value:     rate,
		})
	}

	return result
}

// ============================================================================
// 9. 데모 데이터 및 실행
// ============================================================================

func createDemoStorage() *Storage {
	now := time.Now().UnixMilli()
	min := int64(60 * 1000) // 1분 = 60000ms

	// http_requests_total 시계열 — 4개의 서로 다른 레이블 조합
	return &Storage{
		Series: []TimeSeries{
			{
				Labels: Labels{
					{Name: "__name__", Value: "http_requests_total"},
					{Name: "method", Value: "GET"},
					{Name: "handler", Value: "/api/v1/query"},
					{Name: "instance", Value: "localhost:9090"},
				},
				Points: []TimePoint{
					{T: now - 5*min, V: 100},
					{T: now - 4*min, V: 120},
					{T: now - 3*min, V: 145},
					{T: now - 2*min, V: 170},
					{T: now - 1*min, V: 200},
					{T: now, V: 230},
				},
			},
			{
				Labels: Labels{
					{Name: "__name__", Value: "http_requests_total"},
					{Name: "method", Value: "GET"},
					{Name: "handler", Value: "/api/v1/series"},
					{Name: "instance", Value: "localhost:9090"},
				},
				Points: []TimePoint{
					{T: now - 5*min, V: 50},
					{T: now - 4*min, V: 55},
					{T: now - 3*min, V: 62},
					{T: now - 2*min, V: 70},
					{T: now - 1*min, V: 78},
					{T: now, V: 85},
				},
			},
			{
				Labels: Labels{
					{Name: "__name__", Value: "http_requests_total"},
					{Name: "method", Value: "POST"},
					{Name: "handler", Value: "/api/v1/write"},
					{Name: "instance", Value: "localhost:9090"},
				},
				Points: []TimePoint{
					{T: now - 5*min, V: 200},
					{T: now - 4*min, V: 210},
					{T: now - 3*min, V: 225},
					{T: now - 2*min, V: 240},
					{T: now - 1*min, V: 260},
					{T: now, V: 280},
				},
			},
			{
				Labels: Labels{
					{Name: "__name__", Value: "http_requests_total"},
					{Name: "method", Value: "POST"},
					{Name: "handler", Value: "/api/v1/admin/tsdb"},
					{Name: "instance", Value: "localhost:9090"},
				},
				Points: []TimePoint{
					{T: now - 5*min, V: 10},
					{T: now - 4*min, V: 12},
					{T: now - 3*min, V: 15},
					{T: now - 2*min, V: 18},
					{T: now - 1*min, V: 22},
					{T: now, V: 25},
				},
			},
		},
	}
}

func printVector(v Vector) {
	if len(v) == 0 {
		fmt.Println("  (empty result)")
		return
	}
	for _, s := range v {
		fmt.Printf("  %s => %.4f\n", s.Labels, s.Value)
	}
}

func runQuery(storage *Storage, query string) {
	fmt.Println(strings.Repeat("=", 72))
	fmt.Printf("Query: %s\n", query)
	fmt.Println(strings.Repeat("-", 72))

	// 1. Lexing
	tokens := Lex(query)
	fmt.Println("[1] Tokens:")
	for _, t := range tokens {
		if t.Type == TokenEOF {
			break
		}
		fmt.Printf("    %s\n", t)
	}

	// 2. Parsing
	parser := NewParser(tokens)
	ast := parser.Parse()
	fmt.Println("\n[2] AST:")
	PrintAST(ast, "    ", true)

	// 3. Evaluation
	now := time.Now().UnixMilli()
	ev := &Evaluator{
		Storage:       storage,
		Timestamp:     now,
		LookbackDelta: 5 * 60 * 1000, // 5분 (실제 Prometheus 기본값)
	}

	result := ev.Eval(ast)
	fmt.Println("\n[3] Result:")
	printVector(result)
	fmt.Println()
}

func main() {
	fmt.Println("╔════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║          PromQL Evaluator PoC — Prometheus promql/engine.go           ║")
	fmt.Println("╚════════════════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("Prometheus PromQL 평가 파이프라인:")
	fmt.Println("  Query String → Lexer(토큰화) → Parser(AST 생성) → Evaluator(평가) → Vector")
	fmt.Println()
	fmt.Println("실제 소스코드 참조:")
	fmt.Println("  - Lexer:     promql/parser/lex.go (stateFn 상태 머신)")
	fmt.Println("  - AST:       promql/parser/ast.go (Node/Expr 인터페이스)")
	fmt.Println("  - Parser:    promql/parser/parse.go + generated_parser.y.go")
	fmt.Println("  - Evaluator: promql/engine.go (evaluator.eval() switch)")
	fmt.Println("  - Functions: promql/functions.go (rate → extrapolatedRate)")
	fmt.Println()

	storage := createDemoStorage()

	fmt.Println("사전 구성된 시계열 데이터:")
	fmt.Println("  http_requests_total{method=\"GET\",  handler=\"/api/v1/query\"}      → 100..230 (5분)")
	fmt.Println("  http_requests_total{method=\"GET\",  handler=\"/api/v1/series\"}     → 50..85   (5분)")
	fmt.Println("  http_requests_total{method=\"POST\", handler=\"/api/v1/write\"}      → 200..280 (5분)")
	fmt.Println("  http_requests_total{method=\"POST\", handler=\"/api/v1/admin/tsdb\"} → 10..25   (5분)")
	fmt.Println()

	// 데모 1: 단순 벡터 셀렉터 + 레이블 매처
	runQuery(storage, `http_requests_total{method="GET"}`)

	// 데모 2: 비교 연산 (필터링)
	runQuery(storage, `http_requests_total > 100`)

	// 데모 3: 집계 — sum by (method)
	runQuery(storage, `sum by (method) (http_requests_total)`)

	// 데모 4: 집계 — avg by (method)
	runQuery(storage, `avg by (method) (http_requests_total)`)

	// 데모 5: 집계 — count by (method)
	runQuery(storage, `count by (method) (http_requests_total)`)

	// 데모 6: rate 함수
	runQuery(storage, `rate(http_requests_total[5m])`)

	// 데모 7: 이항 연산 — 벡터 + 스칼라
	runQuery(storage, `http_requests_total * 2`)

	// ===== 평가 파이프라인 상세 설명 =====
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println("PromQL 평가 파이프라인 상세 (engine.go 기준)")
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println()
	fmt.Println(`
┌─────────────┐    ┌──────────┐    ┌─────────┐    ┌───────────┐    ┌────────┐
│ Query String │───>│  Lexer   │───>│ Parser  │───>│ Evaluator │───>│ Result │
│              │    │(토큰화)  │    │(AST생성)│    │ (평가)    │    │(Vector)│
└─────────────┘    └──────────┘    └─────────┘    └───────────┘    └────────┘

Lexer (promql/parser/lex.go):
  - stateFn 패턴: type stateFn func(*Lexer) stateFn
  - 상태 전이: lexStatements → lexInsideBraces → lexNumberOrDuration 등
  - 토큰 타입: IDENTIFIER, NUMBER, STRING, 연산자(+,-,*,/,>,<,==), 키워드(sum,by,rate)

AST (promql/parser/ast.go):
  - Node 인터페이스: String(), Pretty(level), PositionRange()
  - Expr 인터페이스: Type() ValueType, PromQLExpr()
  - 주요 노드:
    ┌─ VectorSelector: Name + LabelMatchers []*labels.Matcher
    ├─ BinaryExpr:     Op + LHS/RHS Expr + VectorMatching
    ├─ AggregateExpr:  Op + Expr + Grouping []string + Without bool
    ├─ Call:           Func *Function + Args []Expr
    ├─ NumberLiteral:  Val float64
    └─ MatrixSelector: VectorSelector + Range time.Duration

Evaluator (promql/engine.go):
  evaluator.eval(ctx, expr) 메서드의 switch 분기:
    case *parser.VectorSelector:
      → checkAndExpandSeriesSet() // storage에서 매칭 시계열 조회
      → evalSeries()             // lookbackDelta 내 최신 샘플 선택
    case *parser.BinaryExpr:
      → eval(LHS), eval(RHS)     // 양쪽 재귀 평가
      → VectorMatching으로 레이블 매칭 후 연산 적용
    case *parser.AggregateExpr:
      → eval(Expr)               // 내부 표현식 평가
      → generateGroupingKey()    // 그룹 키 계산
      → aggregation()            // 그룹별 집계
    case *parser.Call:
      → FunctionCalls[name]()    // 등록된 함수 호출
      → rate(): extrapolatedRate(last-first, 외삽 적용)

LookbackDelta (기본 5분):
  - VectorSelector가 정확한 타임스탬프에 데이터가 없을 때
  - lookbackDelta 시간 이내의 가장 최근 샘플을 반환
  - engine.go: defaultLookbackDelta = 5 * time.Minute`)
	fmt.Println()
}
