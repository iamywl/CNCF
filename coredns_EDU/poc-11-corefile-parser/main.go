package main

import (
	"fmt"
	"strings"
	"unicode"
)

// =============================================================================
// PoC 11: Corefile 파서 (Corefile Parser)
// =============================================================================
// CoreDNS의 설정 파일인 Corefile의 파싱 메커니즘을 시뮬레이션한다.
// CoreDNS는 Caddy 서버의 Caddyfile 파서를 기반으로 Corefile을 파싱하며,
// zone:port { plugin args... } 형식의 블록 구조를 해석한다.
//
// 참조: CoreDNS는 내부적으로 github.com/caddyserver/caddy의 caddyfile 패키지를
//       사용하여 Corefile을 파싱한다.
//       - 토큰화 → 블록 파싱 → ServerBlock 생성 → 플러그인 설정
// =============================================================================

// =============================================================================
// 토큰 정의
// =============================================================================

// TokenType은 토큰의 유형을 나타낸다.
type TokenType int

const (
	TokenWord       TokenType = iota // 일반 단어 (zone명, 플러그인명, 인자)
	TokenOpenBrace                   // {
	TokenCloseBrace                  // }
	TokenNewline                     // 줄바꿈 (블록 구분에 사용)
	TokenEOF                         // 파일 끝
)

func (t TokenType) String() string {
	switch t {
	case TokenWord:
		return "WORD"
	case TokenOpenBrace:
		return "OPEN_BRACE"
	case TokenCloseBrace:
		return "CLOSE_BRACE"
	case TokenNewline:
		return "NEWLINE"
	case TokenEOF:
		return "EOF"
	default:
		return "UNKNOWN"
	}
}

// Token은 파싱의 최소 단위이다.
type Token struct {
	Type  TokenType
	Value string
	Line  int
}

// =============================================================================
// 렉서 (토큰화)
// =============================================================================

// Lexer는 Corefile 텍스트를 토큰으로 분해한다.
type Lexer struct {
	input  string
	pos    int
	line   int
	tokens []Token
}

// NewLexer는 새로운 Lexer를 생성한다.
func NewLexer(input string) *Lexer {
	return &Lexer{
		input:  input,
		pos:    0,
		line:   1,
		tokens: make([]Token, 0),
	}
}

// Tokenize는 입력을 토큰으로 분해한다.
func (l *Lexer) Tokenize() []Token {
	for l.pos < len(l.input) {
		ch := l.input[l.pos]

		switch {
		case ch == '#':
			// 주석: 줄 끝까지 건너뜀
			l.skipComment()

		case ch == '\n':
			l.tokens = append(l.tokens, Token{Type: TokenNewline, Value: "\\n", Line: l.line})
			l.line++
			l.pos++

		case ch == '{':
			l.tokens = append(l.tokens, Token{Type: TokenOpenBrace, Value: "{", Line: l.line})
			l.pos++

		case ch == '}':
			l.tokens = append(l.tokens, Token{Type: TokenCloseBrace, Value: "}", Line: l.line})
			l.pos++

		case ch == '"':
			// 따옴표로 감싼 문자열
			l.readQuotedString()

		case unicode.IsSpace(rune(ch)):
			// 공백 건너뜀 (줄바꿈 제외)
			l.pos++

		default:
			// 일반 단어 읽기
			l.readWord()
		}
	}

	l.tokens = append(l.tokens, Token{Type: TokenEOF, Value: "", Line: l.line})
	return l.tokens
}

func (l *Lexer) skipComment() {
	for l.pos < len(l.input) && l.input[l.pos] != '\n' {
		l.pos++
	}
}

func (l *Lexer) readQuotedString() {
	l.pos++ // 시작 따옴표 건너뜀
	start := l.pos
	for l.pos < len(l.input) && l.input[l.pos] != '"' {
		if l.input[l.pos] == '\n' {
			l.line++
		}
		l.pos++
	}
	value := l.input[start:l.pos]
	if l.pos < len(l.input) {
		l.pos++ // 끝 따옴표 건너뜀
	}
	l.tokens = append(l.tokens, Token{Type: TokenWord, Value: value, Line: l.line})
}

func (l *Lexer) readWord() {
	start := l.pos
	for l.pos < len(l.input) {
		ch := l.input[l.pos]
		if unicode.IsSpace(rune(ch)) || ch == '{' || ch == '}' || ch == '#' {
			break
		}
		l.pos++
	}
	value := l.input[start:l.pos]
	l.tokens = append(l.tokens, Token{Type: TokenWord, Value: value, Line: l.line})
}

// =============================================================================
// AST (구문 트리) 정의
// =============================================================================

// PluginConfig는 플러그인 하나의 설정을 나타낸다.
type PluginConfig struct {
	Name      string     // 플러그인 이름 (예: forward, cache)
	Args      []string   // 인라인 인자
	SubBlocks [][]string // 서브 블록 내 추가 설정
}

// ServerBlock은 하나의 서버 블록을 나타낸다.
// Corefile의 zone:port { ... } 구조에 해당한다.
type ServerBlock struct {
	Zones   []string       // Zone 목록 (예: ["example.com.", "example.org."])
	Port    string         // 포트 (기본 "53")
	Plugins []PluginConfig // 플러그인 설정 목록
}

// CorefileConfig는 전체 Corefile 설정을 나타낸다.
type CorefileConfig struct {
	ServerBlocks []ServerBlock
}

// =============================================================================
// 파서
// =============================================================================

// Parser는 토큰 스트림을 AST로 변환한다.
type Parser struct {
	tokens []Token
	pos    int
}

// NewParser는 새로운 Parser를 생성한다.
func NewParser(tokens []Token) *Parser {
	return &Parser{
		tokens: tokens,
		pos:    0,
	}
}

// Parse는 전체 Corefile을 파싱한다.
func (p *Parser) Parse() (*CorefileConfig, error) {
	config := &CorefileConfig{}

	p.skipNewlines()

	for !p.isAtEnd() {
		block, err := p.parseServerBlock()
		if err != nil {
			return nil, err
		}
		if block != nil {
			config.ServerBlocks = append(config.ServerBlocks, *block)
		}
		p.skipNewlines()
	}

	return config, nil
}

// parseServerBlock은 하나의 서버 블록을 파싱한다.
// 형식: zone1 zone2:port { plugin1 args... \n plugin2 args... }
func (p *Parser) parseServerBlock() (*ServerBlock, error) {
	block := &ServerBlock{
		Port: "53", // 기본 포트
	}

	// Zone/포트 파싱 (여러 zone 가능)
	for p.current().Type == TokenWord {
		zonePort := p.current().Value
		p.advance()

		// zone:port 형식 분리
		if idx := strings.LastIndex(zonePort, ":"); idx > 0 {
			zone := zonePort[:idx]
			port := zonePort[idx+1:]
			if zone == "" {
				zone = "."
			}
			block.Zones = append(block.Zones, normalizeZone(zone))
			block.Port = port
		} else {
			block.Zones = append(block.Zones, normalizeZone(zonePort))
		}

		p.skipNewlines()
	}

	if len(block.Zones) == 0 {
		return nil, nil
	}

	// { 기대
	if p.current().Type != TokenOpenBrace {
		return nil, fmt.Errorf("줄 %d: '{' 기대, '%s' 발견", p.current().Line, p.current().Value)
	}
	p.advance()
	p.skipNewlines()

	// 플러그인 블록 파싱
	for p.current().Type != TokenCloseBrace && !p.isAtEnd() {
		plugin, err := p.parsePlugin()
		if err != nil {
			return nil, err
		}
		if plugin != nil {
			block.Plugins = append(block.Plugins, *plugin)
		}
		p.skipNewlines()
	}

	// } 기대
	if p.current().Type != TokenCloseBrace {
		return nil, fmt.Errorf("줄 %d: '}' 기대, '%s' 발견", p.current().Line, p.current().Value)
	}
	p.advance()

	return block, nil
}

// parsePlugin은 플러그인 설정 한 줄(또는 서브블록)을 파싱한다.
func (p *Parser) parsePlugin() (*PluginConfig, error) {
	if p.current().Type != TokenWord {
		return nil, nil
	}

	plugin := &PluginConfig{
		Name: p.current().Value,
	}
	p.advance()

	// 같은 줄의 인자 수집
	for p.current().Type == TokenWord {
		plugin.Args = append(plugin.Args, p.current().Value)
		p.advance()
	}

	// 서브 블록이 있는 경우
	if p.current().Type == TokenOpenBrace {
		p.advance()
		p.skipNewlines()

		for p.current().Type != TokenCloseBrace && !p.isAtEnd() {
			var subLine []string
			for p.current().Type == TokenWord {
				subLine = append(subLine, p.current().Value)
				p.advance()
			}
			if len(subLine) > 0 {
				plugin.SubBlocks = append(plugin.SubBlocks, subLine)
			}
			p.skipNewlines()
		}

		if p.current().Type == TokenCloseBrace {
			p.advance()
		}
	}

	return plugin, nil
}

func (p *Parser) current() Token {
	if p.pos >= len(p.tokens) {
		return Token{Type: TokenEOF}
	}
	return p.tokens[p.pos]
}

func (p *Parser) advance() {
	if p.pos < len(p.tokens) {
		p.pos++
	}
}

func (p *Parser) skipNewlines() {
	for p.current().Type == TokenNewline {
		p.advance()
	}
}

func (p *Parser) isAtEnd() bool {
	return p.current().Type == TokenEOF
}

// normalizeZone은 zone 이름을 정규화한다 (후행 점 추가).
func normalizeZone(zone string) string {
	if zone == "." {
		return "."
	}
	if !strings.HasSuffix(zone, ".") {
		return zone + "."
	}
	return zone
}

// =============================================================================
// 설정 출력
// =============================================================================

func printConfig(config *CorefileConfig) {
	for i, sb := range config.ServerBlocks {
		fmt.Printf("  서버 블록 #%d:\n", i+1)
		fmt.Printf("    Zone: %s\n", strings.Join(sb.Zones, ", "))
		fmt.Printf("    포트: %s\n", sb.Port)
		fmt.Printf("    플러그인 (%d개):\n", len(sb.Plugins))

		for _, p := range sb.Plugins {
			if len(p.Args) > 0 {
				fmt.Printf("      - %s %s\n", p.Name, strings.Join(p.Args, " "))
			} else {
				fmt.Printf("      - %s\n", p.Name)
			}
			for _, sub := range p.SubBlocks {
				fmt.Printf("          %s\n", strings.Join(sub, " "))
			}
		}
		fmt.Println()
	}
}

func main() {
	fmt.Println("=== CoreDNS Corefile 파서 (Corefile Parser) PoC ===")
	fmt.Println()

	// =========================================================================
	// 1. 기본 Corefile 파싱
	// =========================================================================
	fmt.Println("--- 1. 기본 Corefile 파싱 ---")

	basicCorefile := `
# 기본 CoreDNS 설정
.:53 {
    errors
    log
    health :8080
    cache 30
    forward . 8.8.8.8 8.8.4.4
}
`

	fmt.Println("  입력 Corefile:")
	for _, line := range strings.Split(strings.TrimSpace(basicCorefile), "\n") {
		fmt.Printf("    %s\n", line)
	}
	fmt.Println()

	config, err := parseCorefile(basicCorefile)
	if err != nil {
		fmt.Printf("  파싱 오류: %v\n", err)
		return
	}

	fmt.Println("  파싱 결과:")
	printConfig(config)

	// =========================================================================
	// 2. 다중 Zone Corefile
	// =========================================================================
	fmt.Println("--- 2. 다중 Zone Corefile ---")

	multiZoneCorefile := `
# 여러 zone 정의
example.com:53 {
    file /etc/coredns/example.com.db
    log
    errors
    cache 60
}

example.org:53 {
    file /etc/coredns/example.org.db
    log
    errors
}

# Catch-all (모든 다른 쿼리)
.:53 {
    forward . /etc/resolv.conf
    cache 10
    log
    errors
}
`

	fmt.Println("  입력 Corefile:")
	for _, line := range strings.Split(strings.TrimSpace(multiZoneCorefile), "\n") {
		fmt.Printf("    %s\n", line)
	}
	fmt.Println()

	config, err = parseCorefile(multiZoneCorefile)
	if err != nil {
		fmt.Printf("  파싱 오류: %v\n", err)
		return
	}

	fmt.Println("  파싱 결과:")
	printConfig(config)

	// =========================================================================
	// 3. 서브 블록이 있는 복잡한 Corefile
	// =========================================================================
	fmt.Println("--- 3. 서브 블록이 있는 Corefile ---")

	complexCorefile := `
# 복잡한 설정: 서브 블록, 다중 zone, 커스텀 포트
example.com:1053 {
    rewrite name suffix .internal.example.com .example.com
    forward . 10.0.0.1:53 10.0.0.2:53 {
        policy round_robin
        health_check 5s
        max_fails 3
    }
    cache 300 {
        success 9984 300
        denial 9984 60
    }
    prometheus :9153
    log
    errors
}
`

	fmt.Println("  입력 Corefile:")
	for _, line := range strings.Split(strings.TrimSpace(complexCorefile), "\n") {
		fmt.Printf("    %s\n", line)
	}
	fmt.Println()

	config, err = parseCorefile(complexCorefile)
	if err != nil {
		fmt.Printf("  파싱 오류: %v\n", err)
		return
	}

	fmt.Println("  파싱 결과:")
	printConfig(config)

	// =========================================================================
	// 4. 토큰화 과정 시각화
	// =========================================================================
	fmt.Println("--- 4. 토큰화 과정 시각화 ---")

	tokenDemo := `example.com:53 {
    cache 30
    forward . 8.8.8.8
}`

	fmt.Println("  입력:")
	for _, line := range strings.Split(tokenDemo, "\n") {
		fmt.Printf("    %s\n", line)
	}
	fmt.Println()

	lexer := NewLexer(tokenDemo)
	tokens := lexer.Tokenize()

	fmt.Println("  토큰 목록:")
	fmt.Printf("    %-5s %-15s %s\n", "줄", "타입", "값")
	fmt.Printf("    %-5s %-15s %s\n", "---", "---", "---")
	for _, tok := range tokens {
		displayVal := tok.Value
		if tok.Type == TokenNewline {
			displayVal = "\\n"
		}
		fmt.Printf("    %-5d %-15s %s\n", tok.Line, tok.Type, displayVal)
	}

	// =========================================================================
	// 5. 따옴표 문자열 및 주석 처리
	// =========================================================================
	fmt.Println()
	fmt.Println("--- 5. 따옴표 문자열 및 주석 처리 ---")

	quotedCorefile := `
# 이것은 주석이다
example.com {
    template IN A {
        answer "{{ .Name }} 60 IN A 10.10.10.10"
    }
    # 인라인 주석
    log "query log for example.com"
    errors
}
`

	fmt.Println("  입력 Corefile:")
	for _, line := range strings.Split(strings.TrimSpace(quotedCorefile), "\n") {
		fmt.Printf("    %s\n", line)
	}
	fmt.Println()

	config, err = parseCorefile(quotedCorefile)
	if err != nil {
		fmt.Printf("  파싱 오류: %v\n", err)
		return
	}

	fmt.Println("  파싱 결과:")
	printConfig(config)

	// =========================================================================
	// 6. 오류 처리 테스트
	// =========================================================================
	fmt.Println("--- 6. 오류 처리 ---")

	errorCases := []struct {
		name  string
		input string
	}{
		{"닫는 괄호 누락", "example.com {\n    log\n"},
		{"여는 괄호 누락", "example.com\n    log\n}\n"},
	}

	for _, tc := range errorCases {
		_, err := parseCorefile(tc.input)
		if err != nil {
			fmt.Printf("  [%s] 오류 감지: %v\n", tc.name, err)
		} else {
			fmt.Printf("  [%s] 오류 없이 파싱됨\n", tc.name)
		}
	}
}

// parseCorefile은 Corefile 텍스트를 파싱하여 설정 구조체를 반환한다.
func parseCorefile(input string) (*CorefileConfig, error) {
	lexer := NewLexer(input)
	tokens := lexer.Tokenize()

	parser := NewParser(tokens)
	return parser.Parse()
}
