// Alertmanager Matcher System PoC
//
// Alertmanager의 Matcher 시스템을 시뮬레이션한다.
// pkg/labels/matcher.go의 Matcher/Matchers와 matcher/parse/의 파서를 재현한다.
//
// 핵심 개념:
//   - MatchType: Equal, NotEqual, Regexp, NotRegexp
//   - Matchers: AND 로직 (모든 Matcher가 일치해야 매칭)
//   - 문자열 파서: lexer → token → parser
//   - 정규식 캐싱
//
// 실행: go run main.go

package main

import (
	"fmt"
	"regexp"
	"strings"
)

// MatchType은 매칭 연산자 타입이다.
type MatchType int

const (
	MatchEqual     MatchType = iota // =
	MatchNotEqual                    // !=
	MatchRegexp                      // =~
	MatchNotRegexp                   // !~
)

func (t MatchType) String() string {
	switch t {
	case MatchEqual:
		return "="
	case MatchNotEqual:
		return "!="
	case MatchRegexp:
		return "=~"
	case MatchNotRegexp:
		return "!~"
	}
	return "?"
}

// Matcher는 단일 레이블 매칭 조건이다.
type Matcher struct {
	Type  MatchType
	Name  string
	Value string
	re    *regexp.Regexp // 정규식 캐시
}

// NewMatcher는 새 Matcher를 생성한다.
// 정규식 타입이면 컴파일하여 캐시한다.
func NewMatcher(t MatchType, name, value string) (*Matcher, error) {
	m := &Matcher{Type: t, Name: name, Value: value}

	if t == MatchRegexp || t == MatchNotRegexp {
		// 전체 문자열 매칭을 강제하기 위해 ^(?:...)$ 감싸기
		re, err := regexp.Compile("^(?:" + value + ")$")
		if err != nil {
			return nil, fmt.Errorf("정규식 컴파일 오류: %w", err)
		}
		m.re = re
	}

	return m, nil
}

// Matches는 값이 Matcher 조건에 맞는지 확인한다.
func (m *Matcher) Matches(s string) bool {
	switch m.Type {
	case MatchEqual:
		return s == m.Value
	case MatchNotEqual:
		return s != m.Value
	case MatchRegexp:
		return m.re.MatchString(s)
	case MatchNotRegexp:
		return !m.re.MatchString(s)
	}
	return false
}

// String은 Matcher의 문자열 표현을 반환한다.
func (m *Matcher) String() string {
	return fmt.Sprintf("%s%s%q", m.Name, m.Type, m.Value)
}

// Matchers는 Matcher 슬라이스이다.
type Matchers []*Matcher

// Matches는 모든 Matcher가 LabelSet과 일치하는지 확인한다 (AND).
func (ms Matchers) Matches(lset map[string]string) bool {
	for _, m := range ms {
		if !m.Matches(lset[m.Name]) {
			return false
		}
	}
	return true
}

// MatcherSet은 여러 Matchers 세트이다 (OR 로직).
type MatcherSet []Matchers

// Matches는 하나라도 매칭되면 true를 반환한다 (OR).
func (ms MatcherSet) Matches(lset map[string]string) bool {
	for _, m := range ms {
		if m.Matches(lset) {
			return true
		}
	}
	return false
}

// === 간소화된 Matcher 파서 ===

// tokenKind은 토큰 타입이다.
type tokenKind int

const (
	tokenEOF tokenKind = iota
	tokenOpenBrace
	tokenCloseBrace
	tokenComma
	tokenEquals
	tokenNotEquals
	tokenMatches
	tokenNotMatches
	tokenQuoted
	tokenUnquoted
)

// token은 파서 토큰이다.
type token struct {
	kind  tokenKind
	value string
}

// lexer는 토큰 스캐너이다.
type lexer struct {
	input string
	pos   int
}

func newLexer(input string) *lexer {
	return &lexer{input: input}
}

// scan은 다음 토큰을 반환한다.
func (l *lexer) scan() token {
	l.skipWhitespace()

	if l.pos >= len(l.input) {
		return token{kind: tokenEOF}
	}

	ch := l.input[l.pos]
	switch ch {
	case '{':
		l.pos++
		return token{kind: tokenOpenBrace, value: "{"}
	case '}':
		l.pos++
		return token{kind: tokenCloseBrace, value: "}"}
	case ',':
		l.pos++
		return token{kind: tokenComma, value: ","}
	case '=':
		l.pos++
		if l.pos < len(l.input) && l.input[l.pos] == '~' {
			l.pos++
			return token{kind: tokenMatches, value: "=~"}
		}
		return token{kind: tokenEquals, value: "="}
	case '!':
		l.pos++
		if l.pos < len(l.input) && l.input[l.pos] == '=' {
			l.pos++
			return token{kind: tokenNotEquals, value: "!="}
		}
		if l.pos < len(l.input) && l.input[l.pos] == '~' {
			l.pos++
			return token{kind: tokenNotMatches, value: "!~"}
		}
	case '"':
		return l.scanQuoted()
	}

	return l.scanUnquoted()
}

func (l *lexer) skipWhitespace() {
	for l.pos < len(l.input) && (l.input[l.pos] == ' ' || l.input[l.pos] == '\t') {
		l.pos++
	}
}

func (l *lexer) scanQuoted() token {
	l.pos++ // '"' 건너뛰기
	start := l.pos
	for l.pos < len(l.input) && l.input[l.pos] != '"' {
		l.pos++
	}
	value := l.input[start:l.pos]
	if l.pos < len(l.input) {
		l.pos++ // 닫는 '"'
	}
	return token{kind: tokenQuoted, value: value}
}

func (l *lexer) scanUnquoted() token {
	start := l.pos
	for l.pos < len(l.input) {
		ch := l.input[l.pos]
		if ch == '{' || ch == '}' || ch == ',' || ch == '=' || ch == '!' || ch == '"' || ch == ' ' {
			break
		}
		l.pos++
	}
	return token{kind: tokenUnquoted, value: l.input[start:l.pos]}
}

// ParseMatchers는 문자열을 Matchers로 파싱한다.
// 형식: {name="value", name=~"regex"}
func ParseMatchers(input string) (Matchers, error) {
	l := newLexer(input)
	var matchers Matchers

	// 선택적 { 확인
	tok := l.scan()
	hasBrace := tok.kind == tokenOpenBrace
	if !hasBrace {
		// { 없으면 되돌리기
		l.pos = 0
	}

	for {
		// 이름
		nameTok := l.scan()
		if nameTok.kind == tokenCloseBrace || nameTok.kind == tokenEOF {
			break
		}

		// 연산자
		opTok := l.scan()
		var matchType MatchType
		switch opTok.kind {
		case tokenEquals:
			matchType = MatchEqual
		case tokenNotEquals:
			matchType = MatchNotEqual
		case tokenMatches:
			matchType = MatchRegexp
		case tokenNotMatches:
			matchType = MatchNotRegexp
		default:
			return nil, fmt.Errorf("예상치 않은 연산자: %s", opTok.value)
		}

		// 값
		valTok := l.scan()

		m, err := NewMatcher(matchType, nameTok.value, valTok.value)
		if err != nil {
			return nil, err
		}
		matchers = append(matchers, m)

		// 콤마 또는 끝
		sep := l.scan()
		if sep.kind == tokenCloseBrace || sep.kind == tokenEOF {
			break
		}
	}

	return matchers, nil
}

func main() {
	fmt.Println("=== Alertmanager Matcher System PoC ===")
	fmt.Println()

	// 1. 개별 Matcher 테스트
	fmt.Println("--- 1. 개별 Matcher ---")
	testMatchers := []struct {
		matchType MatchType
		name, value, test string
	}{
		{MatchEqual, "alertname", "HighCPU", "HighCPU"},
		{MatchEqual, "alertname", "HighCPU", "LowCPU"},
		{MatchNotEqual, "severity", "info", "critical"},
		{MatchNotEqual, "severity", "info", "info"},
		{MatchRegexp, "instance", "node-.*", "node-1"},
		{MatchRegexp, "instance", "node-.*", "server-1"},
		{MatchNotRegexp, "cluster", "dev|staging", "prod"},
		{MatchNotRegexp, "cluster", "dev|staging", "dev"},
	}

	for _, tc := range testMatchers {
		m, err := NewMatcher(tc.matchType, tc.name, tc.value)
		if err != nil {
			fmt.Printf("  오류: %v\n", err)
			continue
		}
		result := m.Matches(tc.test)
		fmt.Printf("  %s matches %q → %v\n", m, tc.test, result)
	}
	fmt.Println()

	// 2. Matchers (AND 로직)
	fmt.Println("--- 2. Matchers (AND 로직) ---")
	m1, _ := NewMatcher(MatchEqual, "alertname", "HighCPU")
	m2, _ := NewMatcher(MatchEqual, "severity", "critical")
	matchers := Matchers{m1, m2}

	labels1 := map[string]string{"alertname": "HighCPU", "severity": "critical"}
	labels2 := map[string]string{"alertname": "HighCPU", "severity": "warning"}
	labels3 := map[string]string{"alertname": "HighMemory", "severity": "critical"}

	fmt.Printf("  Matchers: [%s, %s]\n", m1, m2)
	fmt.Printf("  %v → %v (AND: true && true)\n", labels1, matchers.Matches(labels1))
	fmt.Printf("  %v → %v (AND: true && false)\n", labels2, matchers.Matches(labels2))
	fmt.Printf("  %v → %v (AND: false && true)\n", labels3, matchers.Matches(labels3))
	fmt.Println()

	// 3. MatcherSet (OR 로직)
	fmt.Println("--- 3. MatcherSet (OR 로직) ---")
	set1_m1, _ := NewMatcher(MatchEqual, "severity", "critical")
	set2_m1, _ := NewMatcher(MatchEqual, "severity", "warning")
	mset := MatcherSet{Matchers{set1_m1}, Matchers{set2_m1}}

	testSets := []map[string]string{
		{"severity": "critical"},
		{"severity": "warning"},
		{"severity": "info"},
	}
	fmt.Printf("  MatcherSet: [severity=critical] OR [severity=warning]\n")
	for _, ls := range testSets {
		fmt.Printf("  %v → %v\n", ls, mset.Matches(ls))
	}
	fmt.Println()

	// 4. 문자열 파싱
	fmt.Println("--- 4. 문자열 파싱 ---")
	parseTests := []string{
		`{alertname="HighCPU", severity=~"crit.*"}`,
		`{instance!="localhost", cluster!~"dev|staging"}`,
		`alertname="Simple"`,
	}

	for _, input := range parseTests {
		fmt.Printf("  입력: %s\n", input)
		parsed, err := ParseMatchers(input)
		if err != nil {
			fmt.Printf("  파싱 오류: %v\n", err)
			continue
		}
		fmt.Printf("  결과: [")
		for i, m := range parsed {
			if i > 0 {
				fmt.Print(", ")
			}
			fmt.Print(m)
		}
		fmt.Println("]")
	}
	fmt.Println()

	// 5. 정규식 캐싱 확인
	fmt.Println("--- 5. 정규식 최적화 ---")
	regexMatcher, _ := NewMatcher(MatchRegexp, "instance", "node-[0-9]+")
	fmt.Printf("  패턴: %s\n", regexMatcher)
	fmt.Printf("  내부 정규식: %s (^(?:...)$ 감싸기)\n", "^(?:node-[0-9]+)$")

	tests := []string{"node-1", "node-123", "node-abc", "server-1"}
	for _, t := range tests {
		fmt.Printf("  %q → %v\n", t, regexMatcher.Matches(t))
	}

	fmt.Println()
	fmt.Println("=== 동작 원리 요약 ===")
	fmt.Println("1. Matcher: 4가지 연산자 (=, !=, =~, !~)")
	fmt.Println("2. 정규식은 생성 시 컴파일 후 캐시 (매 매칭 시 재컴파일 방지)")
	fmt.Println("3. ^(?:...)$ 감싸기로 전체 문자열 매칭 강제")
	fmt.Println("4. Matchers: AND 로직 (모든 조건 일치)")
	fmt.Println("5. MatcherSet: OR 로직 (하나라도 일치)")
	fmt.Printf("6. 파서: lexer(토큰화) → parser(구조화) → Matcher 생성\n")

	_ = strings.Join // unused import 방지
}
