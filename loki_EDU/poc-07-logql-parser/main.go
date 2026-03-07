package main

import (
	"fmt"
	"strings"
	"unicode"
)

// =============================================================================
// Loki PoC #07: LogQL 파서 - LogQL 쿼리 파싱 및 AST 생성
// =============================================================================
//
// Loki의 LogQL은 로그 데이터를 쿼리하기 위한 전용 언어다.
// 실제 Loki 소스 pkg/logql/syntax/ 에는 lexer(토크나이저)와 recursive descent
// parser가 구현되어 있으며, 파싱 결과를 AST(Abstract Syntax Tree)로 변환한다.
//
// 이 PoC는 LogQL의 핵심 문법 요소를 파싱하는 과정을 시뮬레이션한다:
//   1. Lexer: 입력 문자열을 토큰 스트림으로 변환
//   2. Parser: 재귀 하강 파서로 AST 생성
//   3. 지원 문법:
//      - 스트림 셀렉터:  {app="api", env=~"prod.*"}
//      - 라인 필터:      |= "error", != "debug", |~ "err.*"
//      - 파서:           | json, | logfmt, | regexp "(?P<ip>\\d+)"
//      - 레이블 필터:    | level="error", | status>=400
//
// 실행: go run main.go

// =============================================================================
// 1. 토큰 정의
// =============================================================================

// TokenType은 LogQL 토큰의 종류를 나타낸다.
type TokenType int

const (
	// 리터럴 및 구분자
	TokenEOF        TokenType = iota
	TokenOpenBrace            // {
	TokenCloseBrace           // }
	TokenComma                // ,
	TokenPipe                 // |
	TokenString               // "quoted string"
	TokenIdent                // 식별자 (app, level, json, logfmt 등)
	TokenNumber               // 숫자 (400, 500 등)

	// 비교 연산자
	TokenEq       // =
	TokenNeq      // !=
	TokenRegexEq  // =~
	TokenRegexNeq // !~

	// 라인 필터 연산자
	TokenLineContains    // |=
	TokenLineNotContains // != (라인 필터로도 사용)
	TokenLineRegex       // |~
	TokenLineNotRegex    // !~

	// 숫자 비교 연산자
	TokenGt  // >
	TokenGte // >=
	TokenLt  // <
	TokenLte // <=
)

// tokenTypeNames는 토큰 타입의 문자열 표현을 제공한다.
var tokenTypeNames = map[TokenType]string{
	TokenEOF:             "EOF",
	TokenOpenBrace:       "{",
	TokenCloseBrace:      "}",
	TokenComma:           ",",
	TokenPipe:            "|",
	TokenString:          "STRING",
	TokenIdent:           "IDENT",
	TokenNumber:          "NUMBER",
	TokenEq:              "=",
	TokenNeq:             "!=",
	TokenRegexEq:         "=~",
	TokenRegexNeq:        "!~",
	TokenLineContains:    "|=",
	TokenLineNotContains: "!=",
	TokenLineRegex:       "|~",
	TokenLineNotRegex:    "!~",
	TokenGt:              ">",
	TokenGte:             ">=",
	TokenLt:              "<",
	TokenLte:             "<=",
}

// Token은 Lexer가 생성하는 하나의 토큰이다.
type Token struct {
	Type    TokenType
	Literal string // 토큰의 원래 문자열
	Pos     int    // 입력 문자열 내 위치
}

func (t Token) String() string {
	name := tokenTypeNames[t.Type]
	if t.Literal != "" && t.Type != TokenEOF {
		return fmt.Sprintf("%s(%s)", name, t.Literal)
	}
	return name
}

// =============================================================================
// 2. Lexer (토크나이저)
// =============================================================================
// Loki의 실제 lexer는 pkg/logql/syntax/lexer.go에 구현되어 있다.
// 이 PoC는 LogQL의 주요 토큰을 인식하는 간이 lexer를 구현한다.

// Lexer는 입력 문자열을 토큰 스트림으로 변환한다.
type Lexer struct {
	input   string
	pos     int    // 현재 읽기 위치
	ch      byte   // 현재 문자
	tokens  []Token
}

// NewLexer는 새로운 Lexer를 생성한다.
func NewLexer(input string) *Lexer {
	l := &Lexer{input: input}
	if len(input) > 0 {
		l.ch = input[0]
	}
	return l
}

// advance는 다음 문자로 이동한다.
func (l *Lexer) advance() {
	l.pos++
	if l.pos >= len(l.input) {
		l.ch = 0 // EOF
	} else {
		l.ch = l.input[l.pos]
	}
}

// peek는 다음 문자를 미리 확인한다 (위치는 이동하지 않음).
func (l *Lexer) peek() byte {
	if l.pos+1 >= len(l.input) {
		return 0
	}
	return l.input[l.pos+1]
}

// skipWhitespace는 공백 문자를 건너뛴다.
func (l *Lexer) skipWhitespace() {
	for l.ch != 0 && (l.ch == ' ' || l.ch == '\t' || l.ch == '\n' || l.ch == '\r') {
		l.advance()
	}
}

// readString은 따옴표로 둘러싸인 문자열을 읽는다.
func (l *Lexer) readString() string {
	// 여는 따옴표는 이미 읽었다고 가정
	l.advance() // 여는 따옴표 건너뛰기
	start := l.pos
	for l.ch != 0 && l.ch != '"' {
		if l.ch == '\\' {
			l.advance() // 이스케이프 문자 건너뛰기
		}
		l.advance()
	}
	str := l.input[start:l.pos]
	if l.ch == '"' {
		l.advance() // 닫는 따옴표 건너뛰기
	}
	return str
}

// readBacktickString은 백틱으로 둘러싸인 문자열을 읽는다.
func (l *Lexer) readBacktickString() string {
	l.advance() // 여는 백틱 건너뛰기
	start := l.pos
	for l.ch != 0 && l.ch != '`' {
		l.advance()
	}
	str := l.input[start:l.pos]
	if l.ch == '`' {
		l.advance() // 닫는 백틱 건너뛰기
	}
	return str
}

// readIdent는 식별자를 읽는다.
func (l *Lexer) readIdent() string {
	start := l.pos
	for l.ch != 0 && (unicode.IsLetter(rune(l.ch)) || unicode.IsDigit(rune(l.ch)) || l.ch == '_') {
		l.advance()
	}
	return l.input[start:l.pos]
}

// readNumber는 숫자를 읽는다.
func (l *Lexer) readNumber() string {
	start := l.pos
	for l.ch != 0 && (unicode.IsDigit(rune(l.ch)) || l.ch == '.') {
		l.advance()
	}
	return l.input[start:l.pos]
}

// Tokenize는 입력 문자열 전체를 토큰화한다.
func (l *Lexer) Tokenize() []Token {
	var tokens []Token

	for {
		l.skipWhitespace()
		if l.ch == 0 {
			tokens = append(tokens, Token{Type: TokenEOF, Pos: l.pos})
			break
		}

		pos := l.pos
		switch l.ch {
		case '{':
			tokens = append(tokens, Token{Type: TokenOpenBrace, Literal: "{", Pos: pos})
			l.advance()
		case '}':
			tokens = append(tokens, Token{Type: TokenCloseBrace, Literal: "}", Pos: pos})
			l.advance()
		case ',':
			tokens = append(tokens, Token{Type: TokenComma, Literal: ",", Pos: pos})
			l.advance()
		case '|':
			// |= (라인 포함), |~ (라인 정규식), | (파이프)
			if l.peek() == '=' {
				tokens = append(tokens, Token{Type: TokenLineContains, Literal: "|=", Pos: pos})
				l.advance()
				l.advance()
			} else if l.peek() == '~' {
				tokens = append(tokens, Token{Type: TokenLineRegex, Literal: "|~", Pos: pos})
				l.advance()
				l.advance()
			} else {
				tokens = append(tokens, Token{Type: TokenPipe, Literal: "|", Pos: pos})
				l.advance()
			}
		case '=':
			// = (등호), =~ (정규식 매칭)
			if l.peek() == '~' {
				tokens = append(tokens, Token{Type: TokenRegexEq, Literal: "=~", Pos: pos})
				l.advance()
				l.advance()
			} else {
				tokens = append(tokens, Token{Type: TokenEq, Literal: "=", Pos: pos})
				l.advance()
			}
		case '!':
			// != (부등호), !~ (정규식 불일치)
			if l.peek() == '=' {
				tokens = append(tokens, Token{Type: TokenNeq, Literal: "!=", Pos: pos})
				l.advance()
				l.advance()
			} else if l.peek() == '~' {
				tokens = append(tokens, Token{Type: TokenRegexNeq, Literal: "!~", Pos: pos})
				l.advance()
				l.advance()
			} else {
				l.advance() // 알 수 없는 문자 건너뛰기
			}
		case '>':
			if l.peek() == '=' {
				tokens = append(tokens, Token{Type: TokenGte, Literal: ">=", Pos: pos})
				l.advance()
				l.advance()
			} else {
				tokens = append(tokens, Token{Type: TokenGt, Literal: ">", Pos: pos})
				l.advance()
			}
		case '<':
			if l.peek() == '=' {
				tokens = append(tokens, Token{Type: TokenLte, Literal: "<=", Pos: pos})
				l.advance()
				l.advance()
			} else {
				tokens = append(tokens, Token{Type: TokenLt, Literal: "<", Pos: pos})
				l.advance()
			}
		case '"':
			str := l.readString()
			tokens = append(tokens, Token{Type: TokenString, Literal: str, Pos: pos})
		case '`':
			str := l.readBacktickString()
			tokens = append(tokens, Token{Type: TokenString, Literal: str, Pos: pos})
		default:
			if unicode.IsLetter(rune(l.ch)) || l.ch == '_' {
				ident := l.readIdent()
				tokens = append(tokens, Token{Type: TokenIdent, Literal: ident, Pos: pos})
			} else if unicode.IsDigit(rune(l.ch)) {
				num := l.readNumber()
				tokens = append(tokens, Token{Type: TokenNumber, Literal: num, Pos: pos})
			} else {
				l.advance() // 알 수 없는 문자 건너뛰기
			}
		}
	}

	l.tokens = tokens
	return tokens
}

// =============================================================================
// 3. AST 노드 정의
// =============================================================================
// Loki의 실제 AST는 pkg/logql/syntax/ast.go에 정의되어 있다.
// LogQL 쿼리는 다음과 같은 트리 구조로 표현된다:
//
//   LogQueryExpr
//   ├── StreamSelector: {app="api", env="prod"}
//   └── Pipeline:
//       ├── LineFilter: |= "error"
//       ├── Parser: | json
//       └── LabelFilter: | level="error"

// Node는 AST의 기본 인터페이스이다.
type Node interface {
	// String은 노드를 문자열로 표현한다 (LogQL 쿼리 재구성).
	String() string
	// PrettyPrint는 들여쓰기를 적용한 트리 형태로 출력한다.
	PrettyPrint(indent int) string
}

// MatcherType은 스트림 셀렉터의 매칭 타입이다.
type MatcherType int

const (
	MatchEqual     MatcherType = iota // =
	MatchNotEqual                     // !=
	MatchRegexp                       // =~
	MatchNotRegexp                    // !~
)

func (m MatcherType) String() string {
	switch m {
	case MatchEqual:
		return "="
	case MatchNotEqual:
		return "!="
	case MatchRegexp:
		return "=~"
	case MatchNotRegexp:
		return "!~"
	default:
		return "?"
	}
}

// Matcher는 하나의 레이블 매처를 나타낸다. 예: app="api"
type Matcher struct {
	Name  string      // 레이블 이름
	Type  MatcherType // 매칭 타입
	Value string      // 매칭 값
}

func (m *Matcher) String() string {
	return fmt.Sprintf(`%s%s"%s"`, m.Name, m.Type, m.Value)
}

func (m *Matcher) PrettyPrint(indent int) string {
	prefix := strings.Repeat("  ", indent)
	return fmt.Sprintf("%sMatcher: %s %s \"%s\"", prefix, m.Name, m.Type, m.Value)
}

// StreamSelector는 스트림 셀렉터 노드이다. 예: {app="api", env="prod"}
type StreamSelector struct {
	Matchers []*Matcher
}

func (s *StreamSelector) String() string {
	parts := make([]string, len(s.Matchers))
	for i, m := range s.Matchers {
		parts[i] = m.String()
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func (s *StreamSelector) PrettyPrint(indent int) string {
	prefix := strings.Repeat("  ", indent)
	result := prefix + "StreamSelector:\n"
	for _, m := range s.Matchers {
		result += m.PrettyPrint(indent+1) + "\n"
	}
	return result
}

// LineFilterType은 라인 필터의 종류이다.
type LineFilterType int

const (
	LineFilterContains    LineFilterType = iota // |=  : 포함
	LineFilterNotContains                       // !=  : 미포함
	LineFilterRegex                             // |~  : 정규식 매칭
	LineFilterNotRegex                          // !~  : 정규식 불일치
)

func (f LineFilterType) String() string {
	switch f {
	case LineFilterContains:
		return "|="
	case LineFilterNotContains:
		return "!="
	case LineFilterRegex:
		return "|~"
	case LineFilterNotRegex:
		return "!~"
	default:
		return "?"
	}
}

// LineFilter는 라인 필터 노드이다. 예: |= "error"
type LineFilter struct {
	Type    LineFilterType
	Pattern string
}

func (f *LineFilter) String() string {
	return fmt.Sprintf(`%s "%s"`, f.Type, f.Pattern)
}

func (f *LineFilter) PrettyPrint(indent int) string {
	prefix := strings.Repeat("  ", indent)
	return fmt.Sprintf("%sLineFilter: %s \"%s\"", prefix, f.Type, f.Pattern)
}

// ParserType은 파서 스테이지의 종류이다.
type ParserType int

const (
	ParserJSON   ParserType = iota // | json
	ParserLogfmt                   // | logfmt
	ParserRegexp                   // | regexp "pattern"
	ParserLine                     // | line_format "template"
)

func (p ParserType) String() string {
	switch p {
	case ParserJSON:
		return "json"
	case ParserLogfmt:
		return "logfmt"
	case ParserRegexp:
		return "regexp"
	case ParserLine:
		return "line_format"
	default:
		return "?"
	}
}

// ParserStage는 파서 스테이지 노드이다. 예: | json, | regexp "pattern"
type ParserStage struct {
	Type    ParserType
	Pattern string // regexp인 경우 패턴, 아니면 빈 문자열
}

func (p *ParserStage) String() string {
	if p.Pattern != "" {
		return fmt.Sprintf(`| %s "%s"`, p.Type, p.Pattern)
	}
	return fmt.Sprintf("| %s", p.Type)
}

func (p *ParserStage) PrettyPrint(indent int) string {
	prefix := strings.Repeat("  ", indent)
	if p.Pattern != "" {
		return fmt.Sprintf("%sParserStage: %s (pattern: \"%s\")", prefix, p.Type, p.Pattern)
	}
	return fmt.Sprintf("%sParserStage: %s", prefix, p.Type)
}

// CompareOp은 레이블 필터의 비교 연산자이다.
type CompareOp int

const (
	CompareEq  CompareOp = iota // =
	CompareNeq                  // !=
	CompareGt                   // >
	CompareGte                  // >=
	CompareLt                   // <
	CompareLte                  // <=
)

func (c CompareOp) String() string {
	switch c {
	case CompareEq:
		return "="
	case CompareNeq:
		return "!="
	case CompareGt:
		return ">"
	case CompareGte:
		return ">="
	case CompareLt:
		return "<"
	case CompareLte:
		return "<="
	default:
		return "?"
	}
}

// LabelFilter는 레이블 필터 노드이다. 예: | level="error", | status>=400
type LabelFilter struct {
	Name     string    // 레이블 이름
	Op       CompareOp // 비교 연산자
	Value    string    // 비교 값 (문자열 또는 숫자)
	IsNumber bool      // 숫자 비교인지 여부
}

func (f *LabelFilter) String() string {
	if f.IsNumber {
		return fmt.Sprintf("| %s%s%s", f.Name, f.Op, f.Value)
	}
	return fmt.Sprintf(`| %s%s"%s"`, f.Name, f.Op, f.Value)
}

func (f *LabelFilter) PrettyPrint(indent int) string {
	prefix := strings.Repeat("  ", indent)
	if f.IsNumber {
		return fmt.Sprintf("%sLabelFilter: %s %s %s (numeric)", prefix, f.Name, f.Op, f.Value)
	}
	return fmt.Sprintf("%sLabelFilter: %s %s \"%s\" (string)", prefix, f.Name, f.Op, f.Value)
}

// PipelineStage는 파이프라인의 한 스테이지를 나타내는 인터페이스이다.
type PipelineStage interface {
	Node
	stageType() string
}

func (f *LineFilter) stageType() string   { return "LineFilter" }
func (p *ParserStage) stageType() string  { return "ParserStage" }
func (f *LabelFilter) stageType() string  { return "LabelFilter" }

// LogQueryExpr은 전체 로그 쿼리를 나타내는 최상위 AST 노드이다.
type LogQueryExpr struct {
	Selector *StreamSelector
	Pipeline []PipelineStage
}

func (q *LogQueryExpr) String() string {
	result := q.Selector.String()
	for _, stage := range q.Pipeline {
		result += " " + stage.String()
	}
	return result
}

func (q *LogQueryExpr) PrettyPrint(indent int) string {
	prefix := strings.Repeat("  ", indent)
	result := prefix + "LogQueryExpr:\n"
	result += q.Selector.PrettyPrint(indent + 1)
	if len(q.Pipeline) > 0 {
		result += prefix + "  Pipeline:\n"
		for _, stage := range q.Pipeline {
			result += stage.PrettyPrint(indent+2) + "\n"
		}
	}
	return result
}

// =============================================================================
// 4. Parser (재귀 하강 파서)
// =============================================================================
// Loki의 실제 파서는 pkg/logql/syntax/parser.go에 구현되어 있다.
// 재귀 하강 파서(Recursive Descent Parser)는 각 문법 규칙을 함수로 구현한다.
//
// LogQL 문법 (간이 EBNF):
//   logQuery    = streamSelector { pipelineStage }
//   streamSelector = "{" matcher { "," matcher } "}"
//   matcher     = IDENT ("=" | "!=" | "=~" | "!~") STRING
//   pipelineStage = lineFilter | parserStage | labelFilter
//   lineFilter  = ("|=" | "!=" | "|~" | "!~") STRING
//   parserStage = "|" ("json" | "logfmt" | "regexp" STRING | "line_format" STRING)
//   labelFilter = "|" IDENT ("=" | "!=" | ">" | ">=" | "<" | "<=") (STRING | NUMBER)

// Parser는 토큰 스트림을 AST로 변환한다.
type Parser struct {
	tokens []Token
	pos    int
	errors []string
}

// NewParser는 새로운 Parser를 생성한다.
func NewParser(tokens []Token) *Parser {
	return &Parser{tokens: tokens}
}

// current는 현재 토큰을 반환한다.
func (p *Parser) current() Token {
	if p.pos >= len(p.tokens) {
		return Token{Type: TokenEOF}
	}
	return p.tokens[p.pos]
}

// peek는 다음 토큰을 미리 확인한다.
func (p *Parser) peek() Token {
	if p.pos+1 >= len(p.tokens) {
		return Token{Type: TokenEOF}
	}
	return p.tokens[p.pos+1]
}

// advance는 다음 토큰으로 이동한다.
func (p *Parser) advance() Token {
	tok := p.current()
	p.pos++
	return tok
}

// expect는 특정 타입의 토큰을 기대하고, 맞으면 소비한다.
func (p *Parser) expect(t TokenType) Token {
	tok := p.current()
	if tok.Type != t {
		p.errors = append(p.errors, fmt.Sprintf(
			"위치 %d: %s 토큰을 기대했지만 %s를 발견", tok.Pos, tokenTypeNames[t], tok))
	}
	p.pos++
	return tok
}

// addError는 파싱 에러를 추가한다.
func (p *Parser) addError(msg string) {
	p.errors = append(p.errors, msg)
}

// Parse는 전체 LogQL 쿼리를 파싱한다.
func (p *Parser) Parse() (*LogQueryExpr, []string) {
	expr := p.parseLogQuery()
	return expr, p.errors
}

// parseLogQuery는 최상위 로그 쿼리를 파싱한다.
// logQuery = streamSelector { pipelineStage }
func (p *Parser) parseLogQuery() *LogQueryExpr {
	expr := &LogQueryExpr{}

	// 스트림 셀렉터 파싱
	expr.Selector = p.parseStreamSelector()

	// 파이프라인 스테이지 파싱 (반복)
	for p.current().Type != TokenEOF {
		stage := p.parsePipelineStage()
		if stage == nil {
			break
		}
		expr.Pipeline = append(expr.Pipeline, stage)
	}

	return expr
}

// parseStreamSelector는 스트림 셀렉터를 파싱한다.
// streamSelector = "{" matcher { "," matcher } "}"
func (p *Parser) parseStreamSelector() *StreamSelector {
	selector := &StreamSelector{}

	p.expect(TokenOpenBrace)

	// 첫 번째 매처
	if p.current().Type != TokenCloseBrace {
		matcher := p.parseMatcher()
		if matcher != nil {
			selector.Matchers = append(selector.Matchers, matcher)
		}

		// 추가 매처 (쉼표로 구분)
		for p.current().Type == TokenComma {
			p.advance() // 쉼표 소비
			matcher := p.parseMatcher()
			if matcher != nil {
				selector.Matchers = append(selector.Matchers, matcher)
			}
		}
	}

	p.expect(TokenCloseBrace)

	return selector
}

// parseMatcher는 하나의 레이블 매처를 파싱한다.
// matcher = IDENT ("=" | "!=" | "=~" | "!~") STRING
func (p *Parser) parseMatcher() *Matcher {
	name := p.expect(TokenIdent)
	matcher := &Matcher{Name: name.Literal}

	// 매칭 연산자
	switch p.current().Type {
	case TokenEq:
		matcher.Type = MatchEqual
		p.advance()
	case TokenNeq:
		matcher.Type = MatchNotEqual
		p.advance()
	case TokenRegexEq:
		matcher.Type = MatchRegexp
		p.advance()
	case TokenRegexNeq:
		matcher.Type = MatchNotRegexp
		p.advance()
	default:
		p.addError(fmt.Sprintf("위치 %d: 매칭 연산자를 기대했지만 %s를 발견",
			p.current().Pos, p.current()))
		return nil
	}

	// 매칭 값
	value := p.expect(TokenString)
	matcher.Value = value.Literal

	return matcher
}

// parsePipelineStage는 하나의 파이프라인 스테이지를 파싱한다.
func (p *Parser) parsePipelineStage() PipelineStage {
	tok := p.current()

	switch tok.Type {
	// 라인 필터: |= "pattern", |~ "pattern"
	case TokenLineContains:
		return p.parseLineFilter(LineFilterContains)
	case TokenLineRegex:
		return p.parseLineFilter(LineFilterRegex)

	// != 는 라인 필터 또는 레이블 필터일 수 있음
	// 다음 토큰이 STRING이면 라인 필터
	case TokenNeq:
		if p.peek().Type == TokenString {
			return p.parseLineFilter(LineFilterNotContains)
		}
		return nil

	// !~ 는 라인 필터
	case TokenRegexNeq:
		return p.parseLineFilter(LineFilterNotRegex)

	// 파이프: | 다음에 파서 또는 레이블 필터
	case TokenPipe:
		return p.parsePipeStage()

	default:
		return nil
	}
}

// parseLineFilter는 라인 필터를 파싱한다.
// lineFilter = ("|=" | "!=" | "|~" | "!~") STRING
func (p *Parser) parseLineFilter(filterType LineFilterType) *LineFilter {
	p.advance() // 연산자 소비
	pattern := p.expect(TokenString)
	return &LineFilter{
		Type:    filterType,
		Pattern: pattern.Literal,
	}
}

// parsePipeStage는 | 뒤의 스테이지를 파싱한다.
// 파서 스테이지 또는 레이블 필터
func (p *Parser) parsePipeStage() PipelineStage {
	p.advance() // | 소비

	tok := p.current()

	if tok.Type == TokenIdent {
		switch tok.Literal {
		// 파서 스테이지: json, logfmt
		case "json":
			p.advance()
			return &ParserStage{Type: ParserJSON}
		case "logfmt":
			p.advance()
			return &ParserStage{Type: ParserLogfmt}
		case "regexp":
			p.advance()
			pattern := p.expect(TokenString)
			return &ParserStage{Type: ParserRegexp, Pattern: pattern.Literal}
		case "line_format":
			p.advance()
			pattern := p.expect(TokenString)
			return &ParserStage{Type: ParserLine, Pattern: pattern.Literal}
		default:
			// 레이블 필터: | level="error", | status>=400
			return p.parseLabelFilter()
		}
	}

	p.addError(fmt.Sprintf("위치 %d: 파이프 스테이지 시작을 기대했지만 %s를 발견",
		tok.Pos, tok))
	return nil
}

// parseLabelFilter는 레이블 필터를 파싱한다.
// labelFilter = IDENT ("=" | "!=" | ">" | ">=" | "<" | "<=") (STRING | NUMBER)
func (p *Parser) parseLabelFilter() *LabelFilter {
	name := p.advance() // IDENT 이미 확인됨

	filter := &LabelFilter{Name: name.Literal}

	// 비교 연산자
	switch p.current().Type {
	case TokenEq:
		filter.Op = CompareEq
		p.advance()
	case TokenNeq:
		filter.Op = CompareNeq
		p.advance()
	case TokenGt:
		filter.Op = CompareGt
		p.advance()
	case TokenGte:
		filter.Op = CompareGte
		p.advance()
	case TokenLt:
		filter.Op = CompareLt
		p.advance()
	case TokenLte:
		filter.Op = CompareLte
		p.advance()
	default:
		p.addError(fmt.Sprintf("위치 %d: 비교 연산자를 기대했지만 %s를 발견",
			p.current().Pos, p.current()))
		return nil
	}

	// 비교 값 (문자열 또는 숫자)
	if p.current().Type == TokenNumber {
		filter.Value = p.current().Literal
		filter.IsNumber = true
		p.advance()
	} else if p.current().Type == TokenString {
		filter.Value = p.current().Literal
		filter.IsNumber = false
		p.advance()
	} else {
		p.addError(fmt.Sprintf("위치 %d: 값을 기대했지만 %s를 발견",
			p.current().Pos, p.current()))
		return nil
	}

	return filter
}

// =============================================================================
// 5. 쿼리 평가기 (간이)
// =============================================================================
// 파싱된 AST를 사용해 실제 로그 라인을 필터링하는 간이 평가기

// LogEntry는 하나의 로그 엔트리를 나타낸다.
type LogEntry struct {
	Line   string
	Labels map[string]string
}

// evaluateLineFilter는 라인 필터를 평가한다.
func evaluateLineFilter(entry *LogEntry, filter *LineFilter) bool {
	switch filter.Type {
	case LineFilterContains:
		return strings.Contains(entry.Line, filter.Pattern)
	case LineFilterNotContains:
		return !strings.Contains(entry.Line, filter.Pattern)
	default:
		return true // 정규식 필터는 단순화를 위해 항상 통과
	}
}

// evaluateLabelFilter는 레이블 필터를 평가한다.
func evaluateLabelFilter(entry *LogEntry, filter *LabelFilter) bool {
	val, ok := entry.Labels[filter.Name]
	if !ok {
		return false
	}
	switch filter.Op {
	case CompareEq:
		return val == filter.Value
	case CompareNeq:
		return val != filter.Value
	default:
		return true
	}
}

// evaluateStreamSelector는 스트림 셀렉터를 평가한다.
func evaluateStreamSelector(entry *LogEntry, selector *StreamSelector) bool {
	for _, m := range selector.Matchers {
		val, ok := entry.Labels[m.Name]
		if !ok {
			return false
		}
		switch m.Type {
		case MatchEqual:
			if val != m.Value {
				return false
			}
		case MatchNotEqual:
			if val == m.Value {
				return false
			}
		default:
			// 정규식 매칭은 단순화를 위해 항상 통과
		}
	}
	return true
}

// =============================================================================
// 6. 메인 함수 - 파서 시연
// =============================================================================

func main() {
	fmt.Println("=== Loki PoC #07: LogQL 파서 ===")
	fmt.Println()

	// 테스트 쿼리 목록
	queries := []string{
		// 기본 스트림 셀렉터
		`{app="api"}`,

		// 스트림 셀렉터 + 라인 필터
		`{app="api", env="prod"} |= "error"`,

		// 복잡한 파이프라인
		`{app="api"} |= "error" != "debug" | json | level="error"`,

		// 정규식 매칭 + 파서
		`{namespace=~"prod.*"} |~ "HTTP/1\\.[01]" | logfmt | status>="400"`,

		// regexp 파서
		`{app="nginx"} | regexp "(?P<ip>\\d+\\.\\d+\\.\\d+\\.\\d+)" | ip!="127.0.0.1"`,

		// 다중 필터 + line_format
		`{job="varlogs"} |= "error" | json | level!="info" | line_format "{{.level}}: {{.msg}}"`,
	}

	for i, query := range queries {
		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
		fmt.Printf("쿼리 #%d: %s\n", i+1, query)
		fmt.Println()

		// --- Lexer 단계 ---
		lexer := NewLexer(query)
		tokens := lexer.Tokenize()

		fmt.Println("[1] 토큰화 (Lexer) 결과:")
		for j, tok := range tokens {
			if tok.Type == TokenEOF {
				continue
			}
			fmt.Printf("    토큰[%d]: %-20s (위치: %d)\n", j, tok, tok.Pos)
		}
		fmt.Println()

		// --- Parser 단계 ---
		parser := NewParser(tokens)
		ast, errors := parser.Parse()

		if len(errors) > 0 {
			fmt.Println("[!] 파싱 에러:")
			for _, e := range errors {
				fmt.Printf("    - %s\n", e)
			}
			fmt.Println()
			continue
		}

		fmt.Println("[2] AST (Abstract Syntax Tree):")
		fmt.Print(ast.PrettyPrint(2))
		fmt.Println()

		// --- 쿼리 재구성 ---
		fmt.Printf("[3] 재구성된 쿼리: %s\n", ast.String())
		fmt.Println()
	}

	// =============================================================================
	// 7. AST 기반 쿼리 평가 시연
	// =============================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("=== AST 기반 쿼리 평가 시연 ===")
	fmt.Println()

	// 샘플 로그 엔트리
	entries := []LogEntry{
		{Line: `{"level":"error","msg":"connection refused","status":500}`, Labels: map[string]string{"app": "api", "env": "prod"}},
		{Line: `{"level":"info","msg":"request completed","status":200}`, Labels: map[string]string{"app": "api", "env": "prod"}},
		{Line: `{"level":"error","msg":"timeout exceeded","status":504}`, Labels: map[string]string{"app": "api", "env": "staging"}},
		{Line: `{"level":"debug","msg":"debug trace","status":200}`, Labels: map[string]string{"app": "api", "env": "prod"}},
		{Line: `{"level":"error","msg":"null pointer","status":500}`, Labels: map[string]string{"app": "web", "env": "prod"}},
	}

	evalQuery := `{app="api", env="prod"} |= "error" != "debug"`
	fmt.Printf("평가 쿼리: %s\n\n", evalQuery)

	lexer := NewLexer(evalQuery)
	tokens := lexer.Tokenize()
	parser := NewParser(tokens)
	ast, _ := parser.Parse()

	fmt.Println("로그 필터링 결과:")
	matchCount := 0
	for _, entry := range entries {
		// 스트림 셀렉터 평가
		if !evaluateStreamSelector(&entry, ast.Selector) {
			continue
		}

		// 파이프라인 평가
		passed := true
		for _, stage := range ast.Pipeline {
			switch s := stage.(type) {
			case *LineFilter:
				if !evaluateLineFilter(&entry, s) {
					passed = false
				}
			case *LabelFilter:
				if !evaluateLabelFilter(&entry, s) {
					passed = false
				}
			}
			if !passed {
				break
			}
		}

		if passed {
			matchCount++
			fmt.Printf("  [MATCH] labels=%v line=%s\n", entry.Labels, entry.Line)
		} else {
			fmt.Printf("  [SKIP]  labels=%v line=%s\n", entry.Labels, entry.Line)
		}
	}
	fmt.Printf("\n총 %d/%d 엔트리 매칭\n", matchCount, len(entries))

	// =============================================================================
	// 8. 파서 구조 요약
	// =============================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("=== LogQL 파서 구조 요약 ===")
	fmt.Println()
	fmt.Println("Loki의 LogQL 파싱 파이프라인:")
	fmt.Println()
	fmt.Println("  입력 쿼리 문자열")
	fmt.Println("       │")
	fmt.Println("       ▼")
	fmt.Println("  ┌─────────┐    문자 단위로 토큰 분류")
	fmt.Println("  │  Lexer  │    (식별자, 연산자, 문자열 등)")
	fmt.Println("  └────┬────┘")
	fmt.Println("       │ []Token")
	fmt.Println("       ▼")
	fmt.Println("  ┌─────────┐    재귀 하강 파서로 문법 규칙 적용")
	fmt.Println("  │ Parser  │    (스트림셀렉터, 라인필터, 파서, 레이블필터)")
	fmt.Println("  └────┬────┘")
	fmt.Println("       │ AST")
	fmt.Println("       ▼")
	fmt.Println("  ┌──────────┐   AST를 순회하며 쿼리 실행 계획 생성")
	fmt.Println("  │ Evaluator│   (실제 Loki: pipeline.Pipeline으로 변환)")
	fmt.Println("  └──────────┘")
	fmt.Println()
	fmt.Println("핵심 포인트:")
	fmt.Println("  - Lexer: 문자열 → 토큰 스트림 (O(n) 단일 패스)")
	fmt.Println("  - Parser: 토큰 → AST (재귀 하강, LL(1) 파싱)")
	fmt.Println("  - AST: 쿼리의 구조적 표현 (재구성, 최적화, 실행에 활용)")
	fmt.Println("  - Loki 실제 코드: pkg/logql/syntax/{lexer,parser,ast}.go")
}
