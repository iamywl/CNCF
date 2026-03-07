package main

import (
	"fmt"
	"strings"
	"unicode"
)

// =============================================================================
// Terraform HCL 파서 시뮬레이션
// =============================================================================
//
// HCL(HashiCorp Configuration Language)은 Terraform 설정 파일의 구문이다.
// 실제 HCL 파서는 별도 라이브러리(github.com/hashicorp/hcl/v2)로 구현되어 있다.
//
// 이 PoC에서는 간단한 HCL 파서를 직접 구현하여 핵심 개념을 이해한다:
//   1. 토큰화(Lexing/Tokenizing)
//   2. 블록 파싱(Block Parsing)
//   3. 속성(Attribute) 추출
//   4. 중첩 블록(Nested Block) 지원
//   5. 모듈 구조 구축

// =============================================================================
// 1. 토큰 정의
// =============================================================================

// TokenType은 토큰의 종류를 나타낸다.
type TokenType int

const (
	TokenIdent       TokenType = iota // 식별자 (resource, variable, ...)
	TokenString                       // 문자열 ("...")
	TokenNumber                       // 숫자
	TokenEquals                       // =
	TokenOpenBrace                    // {
	TokenCloseBrace                   // }
	TokenNewline                      // 줄바꿈
	TokenBool                         // true/false
	TokenOpenBracket                  // [
	TokenCloseBracket                 // ]
	TokenComma                        // ,
	TokenEOF                          // 파일 끝
)

var tokenNames = map[TokenType]string{
	TokenIdent:        "IDENT",
	TokenString:       "STRING",
	TokenNumber:       "NUMBER",
	TokenEquals:       "EQUALS",
	TokenOpenBrace:    "LBRACE",
	TokenCloseBrace:   "RBRACE",
	TokenNewline:      "NEWLINE",
	TokenBool:         "BOOL",
	TokenOpenBracket:  "LBRACKET",
	TokenCloseBracket: "RBRACKET",
	TokenComma:        "COMMA",
	TokenEOF:          "EOF",
}

// Token은 하나의 토큰을 나타낸다.
type Token struct {
	Type  TokenType
	Value string
	Line  int
}

func (t Token) String() string {
	return fmt.Sprintf("<%s:%q>", tokenNames[t.Type], t.Value)
}

// =============================================================================
// 2. 렉서 (Lexer/Tokenizer)
// =============================================================================

// Lexer는 HCL 소스 문자열을 토큰 스트림으로 변환한다.
type Lexer struct {
	input   []rune
	pos     int
	line    int
	tokens  []Token
}

// NewLexer는 새로운 렉서를 생성한다.
func NewLexer(input string) *Lexer {
	return &Lexer{
		input: []rune(input),
		pos:   0,
		line:  1,
	}
}

// Tokenize는 입력을 토큰으로 분해한다.
func (l *Lexer) Tokenize() []Token {
	for l.pos < len(l.input) {
		ch := l.input[l.pos]

		switch {
		case ch == '#' || (ch == '/' && l.pos+1 < len(l.input) && l.input[l.pos+1] == '/'):
			// 주석 건너뛰기
			for l.pos < len(l.input) && l.input[l.pos] != '\n' {
				l.pos++
			}

		case ch == '\n':
			l.tokens = append(l.tokens, Token{Type: TokenNewline, Value: "\\n", Line: l.line})
			l.line++
			l.pos++

		case ch == ' ' || ch == '\t' || ch == '\r':
			l.pos++

		case ch == '=':
			l.tokens = append(l.tokens, Token{Type: TokenEquals, Value: "=", Line: l.line})
			l.pos++

		case ch == '{':
			l.tokens = append(l.tokens, Token{Type: TokenOpenBrace, Value: "{", Line: l.line})
			l.pos++

		case ch == '}':
			l.tokens = append(l.tokens, Token{Type: TokenCloseBrace, Value: "}", Line: l.line})
			l.pos++

		case ch == '[':
			l.tokens = append(l.tokens, Token{Type: TokenOpenBracket, Value: "[", Line: l.line})
			l.pos++

		case ch == ']':
			l.tokens = append(l.tokens, Token{Type: TokenCloseBracket, Value: "]", Line: l.line})
			l.pos++

		case ch == ',':
			l.tokens = append(l.tokens, Token{Type: TokenComma, Value: ",", Line: l.line})
			l.pos++

		case ch == '"':
			l.readString()

		case unicode.IsDigit(ch) || ch == '-':
			l.readNumber()

		case unicode.IsLetter(ch) || ch == '_':
			l.readIdent()

		default:
			l.pos++
		}
	}

	l.tokens = append(l.tokens, Token{Type: TokenEOF, Value: "", Line: l.line})
	return l.tokens
}

func (l *Lexer) readString() {
	l.pos++ // 시작 "
	start := l.pos
	for l.pos < len(l.input) && l.input[l.pos] != '"' {
		l.pos++
	}
	value := string(l.input[start:l.pos])
	l.pos++ // 끝 "
	l.tokens = append(l.tokens, Token{Type: TokenString, Value: value, Line: l.line})
}

func (l *Lexer) readNumber() {
	start := l.pos
	if l.input[l.pos] == '-' {
		l.pos++
	}
	for l.pos < len(l.input) && (unicode.IsDigit(l.input[l.pos]) || l.input[l.pos] == '.') {
		l.pos++
	}
	l.tokens = append(l.tokens, Token{Type: TokenNumber, Value: string(l.input[start:l.pos]), Line: l.line})
}

func (l *Lexer) readIdent() {
	start := l.pos
	for l.pos < len(l.input) && (unicode.IsLetter(l.input[l.pos]) || unicode.IsDigit(l.input[l.pos]) || l.input[l.pos] == '_' || l.input[l.pos] == '-' || l.input[l.pos] == '.') {
		l.pos++
	}
	value := string(l.input[start:l.pos])

	if value == "true" || value == "false" {
		l.tokens = append(l.tokens, Token{Type: TokenBool, Value: value, Line: l.line})
	} else {
		l.tokens = append(l.tokens, Token{Type: TokenIdent, Value: value, Line: l.line})
	}
}

// =============================================================================
// 3. AST(Abstract Syntax Tree) 노드 정의
// =============================================================================

// Attribute는 key = value 쌍을 나타낸다.
type Attribute struct {
	Key   string
	Value string
}

// Block은 HCL 블록을 나타낸다.
// 예: resource "aws_instance" "web" { ... }
type Block struct {
	Type       string       // resource, variable, output, provider, ...
	Labels     []string     // 블록 라벨 (예: "aws_instance", "web")
	Attributes []Attribute  // key = value 속성들
	SubBlocks  []Block      // 중첩 블록 (lifecycle, provisioner 등)
}

// Module은 하나의 Terraform 모듈(설정 파일 집합)을 나타낸다.
type Module struct {
	Resources []Block
	Variables []Block
	Outputs   []Block
	Providers []Block
	Data      []Block
	Locals    []Block
}

// =============================================================================
// 4. 파서 (Parser)
// =============================================================================

// Parser는 토큰 스트림을 AST로 변환한다.
type Parser struct {
	tokens []Token
	pos    int
}

// NewParser는 새로운 파서를 생성한다.
func NewParser(tokens []Token) *Parser {
	return &Parser{
		tokens: tokens,
		pos:    0,
	}
}

func (p *Parser) current() Token {
	if p.pos >= len(p.tokens) {
		return Token{Type: TokenEOF}
	}
	return p.tokens[p.pos]
}

func (p *Parser) advance() Token {
	t := p.current()
	p.pos++
	return t
}

func (p *Parser) skipNewlines() {
	for p.current().Type == TokenNewline {
		p.advance()
	}
}

// Parse는 전체 파일을 파싱하여 Module을 반환한다.
func (p *Parser) Parse() *Module {
	module := &Module{}

	for p.current().Type != TokenEOF {
		p.skipNewlines()

		if p.current().Type == TokenEOF {
			break
		}

		if p.current().Type == TokenIdent {
			block := p.parseBlock()
			if block == nil {
				continue
			}

			switch block.Type {
			case "resource":
				module.Resources = append(module.Resources, *block)
			case "variable":
				module.Variables = append(module.Variables, *block)
			case "output":
				module.Outputs = append(module.Outputs, *block)
			case "provider":
				module.Providers = append(module.Providers, *block)
			case "data":
				module.Data = append(module.Data, *block)
			case "locals":
				module.Locals = append(module.Locals, *block)
			}
		} else {
			p.advance()
		}
	}

	return module
}

// parseBlock은 하나의 블록을 파싱한다.
func (p *Parser) parseBlock() *Block {
	block := &Block{}

	// 블록 타입 읽기
	if p.current().Type != TokenIdent {
		return nil
	}
	block.Type = p.advance().Value

	// 라벨 읽기 (문자열 리터럴)
	for p.current().Type == TokenString {
		block.Labels = append(block.Labels, p.advance().Value)
	}

	p.skipNewlines()

	// 본문 파싱 { ... }
	if p.current().Type != TokenOpenBrace {
		return nil
	}
	p.advance() // {

	for p.current().Type != TokenCloseBrace && p.current().Type != TokenEOF {
		p.skipNewlines()

		if p.current().Type == TokenCloseBrace {
			break
		}

		if p.current().Type == TokenIdent {
			key := p.current().Value
			nextIdx := p.pos + 1

			// 다음 유효 토큰 찾기 (뉴라인 건너뛰기)
			for nextIdx < len(p.tokens) && p.tokens[nextIdx].Type == TokenNewline {
				nextIdx++
			}

			if nextIdx < len(p.tokens) && p.tokens[nextIdx].Type == TokenEquals {
				// 속성: key = value
				p.advance() // key
				// 뉴라인 건너뛰기
				p.skipNewlines()
				p.advance() // =
				p.skipNewlines()
				value := p.parseValue()
				block.Attributes = append(block.Attributes, Attribute{Key: key, Value: value})
			} else if nextIdx < len(p.tokens) && (p.tokens[nextIdx].Type == TokenOpenBrace || p.tokens[nextIdx].Type == TokenString) {
				// 중첩 블록
				subBlock := p.parseBlock()
				if subBlock != nil {
					block.SubBlocks = append(block.SubBlocks, *subBlock)
				}
			} else {
				p.advance()
			}
		} else {
			p.advance()
		}
	}

	if p.current().Type == TokenCloseBrace {
		p.advance() // }
	}

	return block
}

// parseValue는 속성 값을 파싱한다.
func (p *Parser) parseValue() string {
	switch p.current().Type {
	case TokenString:
		return p.advance().Value
	case TokenNumber:
		return p.advance().Value
	case TokenBool:
		return p.advance().Value
	case TokenIdent:
		return p.advance().Value
	case TokenOpenBracket:
		return p.parseList()
	default:
		return p.advance().Value
	}
}

// parseList는 리스트 값을 파싱한다 [a, b, c]
func (p *Parser) parseList() string {
	p.advance() // [
	var items []string
	for p.current().Type != TokenCloseBracket && p.current().Type != TokenEOF {
		p.skipNewlines()
		if p.current().Type == TokenCloseBracket {
			break
		}
		items = append(items, p.parseValue())
		p.skipNewlines()
		if p.current().Type == TokenComma {
			p.advance()
		}
	}
	if p.current().Type == TokenCloseBracket {
		p.advance() // ]
	}
	return "[" + strings.Join(items, ", ") + "]"
}

// =============================================================================
// 5. 출력 함수
// =============================================================================

func printBlock(block Block, indent int) {
	prefix := strings.Repeat("  ", indent)
	labels := ""
	if len(block.Labels) > 0 {
		labels = " " + strings.Join(block.Labels, " / ")
	}
	fmt.Printf("%s[%s]%s\n", prefix, block.Type, labels)

	for _, attr := range block.Attributes {
		fmt.Printf("%s  %-20s = %s\n", prefix, attr.Key, attr.Value)
	}

	for _, sub := range block.SubBlocks {
		printBlock(sub, indent+1)
	}
}

func printModule(mod *Module) {
	if len(mod.Providers) > 0 {
		fmt.Println("  ┌── Providers ──────────────────────────────┐")
		for _, p := range mod.Providers {
			printBlock(p, 2)
		}
		fmt.Println("  └───────────────────────────────────────────┘")
		fmt.Println()
	}

	if len(mod.Variables) > 0 {
		fmt.Println("  ┌── Variables ──────────────────────────────┐")
		for _, v := range mod.Variables {
			printBlock(v, 2)
		}
		fmt.Println("  └───────────────────────────────────────────┘")
		fmt.Println()
	}

	if len(mod.Resources) > 0 {
		fmt.Println("  ┌── Resources ──────────────────────────────┐")
		for _, r := range mod.Resources {
			printBlock(r, 2)
		}
		fmt.Println("  └───────────────────────────────────────────┘")
		fmt.Println()
	}

	if len(mod.Data) > 0 {
		fmt.Println("  ┌── Data Sources ───────────────────────────┐")
		for _, d := range mod.Data {
			printBlock(d, 2)
		}
		fmt.Println("  └───────────────────────────────────────────┘")
		fmt.Println()
	}

	if len(mod.Outputs) > 0 {
		fmt.Println("  ┌── Outputs ────────────────────────────────┐")
		for _, o := range mod.Outputs {
			printBlock(o, 2)
		}
		fmt.Println("  └───────────────────────────────────────────┘")
	}
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║   Terraform HCL 파서 시뮬레이션                           ║")
	fmt.Println("║   실제 코드: github.com/hashicorp/hcl/v2                ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()

	// =========================================================================
	// 샘플 HCL 설정
	// =========================================================================
	sampleHCL := `
# AWS 프로바이더 설정
provider "aws" {
  region  = "ap-northeast-2"
  profile = "production"
}

# 입력 변수 정의
variable "instance_type" {
  description = "EC2 인스턴스 타입"
  type        = "string"
  default     = "t3.micro"
}

variable "environment" {
  description = "배포 환경"
  type        = "string"
  default     = "production"
}

# VPC 리소스
resource "aws_vpc" "main" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_hostnames = true
  enable_dns_support   = true
}

# 서브넷 리소스
resource "aws_subnet" "public" {
  vpc_id            = "aws_vpc.main.id"
  cidr_block        = "10.0.1.0/24"
  availability_zone = "ap-northeast-2a"
}

# 보안 그룹
resource "aws_security_group" "web" {
  name        = "web-sg"
  description = "웹 서버 보안 그룹"
  vpc_id      = "aws_vpc.main.id"
}

# EC2 인스턴스
resource "aws_instance" "web" {
  ami                    = "ami-0c55b159cbfafe1f0"
  instance_type          = "var.instance_type"
  subnet_id              = "aws_subnet.public.id"
  vpc_security_group_ids = ["aws_security_group.web.id"]

  lifecycle {
    create_before_destroy = true
    prevent_destroy       = false
  }

  provisioner "remote-exec" {
    command = "echo Hello"
  }
}

# 데이터 소스
data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"]
}

# 출력값
output "instance_id" {
  description = "EC2 인스턴스 ID"
  value       = "aws_instance.web.id"
}

output "vpc_id" {
  description = "VPC ID"
  value       = "aws_vpc.main.id"
}
`

	// =========================================================================
	// 데모 1: 토큰화 (Lexing)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 1: 토큰화 (Lexing/Tokenizing)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	lexer := NewLexer(sampleHCL)
	tokens := lexer.Tokenize()

	// 처음 30개 토큰만 출력
	fmt.Println("  처음 30개 토큰:")
	count := 0
	for _, t := range tokens {
		if t.Type == TokenNewline {
			continue
		}
		if count >= 30 {
			break
		}
		fmt.Printf("    %s\n", t)
		count++
	}
	fmt.Printf("\n  총 토큰 수: %d (뉴라인 포함)\n", len(tokens))
	fmt.Println()

	// =========================================================================
	// 데모 2: 파싱 (Parsing)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 2: 파싱 결과 (Parsed Module)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	parser := NewParser(tokens)
	module := parser.Parse()

	printModule(module)

	// =========================================================================
	// 데모 3: 통계 및 분석
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 3: 모듈 통계")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	fmt.Printf("  Provider 블록:  %d개\n", len(module.Providers))
	fmt.Printf("  Variable 블록:  %d개\n", len(module.Variables))
	fmt.Printf("  Resource 블록:  %d개\n", len(module.Resources))
	fmt.Printf("  Data 블록:      %d개\n", len(module.Data))
	fmt.Printf("  Output 블록:    %d개\n", len(module.Outputs))
	fmt.Println()

	// 리소스 타입별 분석
	fmt.Println("  리소스 타입 분석:")
	for _, r := range module.Resources {
		if len(r.Labels) >= 2 {
			fmt.Printf("    %-30s → 속성 %d개, 중첩블록 %d개\n",
				r.Labels[0]+"."+r.Labels[1],
				len(r.Attributes),
				len(r.SubBlocks))
		}
	}

	// 중첩 블록 분석
	fmt.Println()
	fmt.Println("  중첩 블록(Nested Block) 분석:")
	for _, r := range module.Resources {
		for _, sub := range r.SubBlocks {
			labels := ""
			if len(r.Labels) >= 2 {
				labels = r.Labels[0] + "." + r.Labels[1]
			}
			fmt.Printf("    %s → %s (속성 %d개)\n",
				labels, sub.Type, len(sub.Attributes))
		}
	}

	// =========================================================================
	// 데모 4: 참조 추출
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 4: 참조(Reference) 추출")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	fmt.Println("  리소스 간 참조 관계:")
	for _, r := range module.Resources {
		if len(r.Labels) < 2 {
			continue
		}
		resourceAddr := r.Labels[0] + "." + r.Labels[1]
		for _, attr := range r.Attributes {
			// 다른 리소스에 대한 참조 탐지
			for _, other := range module.Resources {
				if len(other.Labels) < 2 {
					continue
				}
				otherAddr := other.Labels[0] + "." + other.Labels[1]
				if otherAddr != resourceAddr && strings.Contains(attr.Value, otherAddr) {
					fmt.Printf("    %s.%s → %s\n", resourceAddr, attr.Key, otherAddr)
				}
			}
			// 변수 참조 탐지
			if strings.Contains(attr.Value, "var.") {
				fmt.Printf("    %s.%s → %s (변수 참조)\n", resourceAddr, attr.Key, attr.Value)
			}
		}
	}

	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  핵심 포인트 정리")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  1. HCL은 블록(Block) 기반 구조: type label1 label2 { body }")
	fmt.Println("  2. 속성(Attribute)은 key = value 형태")
	fmt.Println("  3. 중첩 블록(Nested Block)으로 lifecycle, provisioner 등 표현")
	fmt.Println("  4. 파싱 결과로 Module 구조체(Resources, Variables, Outputs 등) 생성")
	fmt.Println("  5. 참조(Reference) 분석으로 리소스 간 의존 관계 추출")
}
