// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble 출력 포맷터 전략 패턴
//
// Hubble CLI는 여러 출력 형식을 지원합니다:
//   - compact: 한 줄 요약 (기본값)
//   - json: JSON 형식 (파이프라인 처리용)
//   - jsonpb: Protobuf JSON 형식
//   - dict: 키-값 사전 형식 (상세 보기)
//   - tab: 탭 정렬 테이블 형식
//
// 이 패턴은 Strategy 디자인 패턴입니다.
//
// 실행: go run main.go

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// ========================================
// 1. 데이터 타입
// ========================================

type Flow struct {
	Timestamp   time.Time `json:"timestamp"`
	Source      string    `json:"source"`
	Destination string    `json:"destination"`
	Verdict     string    `json:"verdict"`
	Protocol    string    `json:"protocol"`
	Port        int       `json:"port"`
	Namespace   string    `json:"namespace"`
	L7Type      string    `json:"l7_type,omitempty"`
	HTTPMethod  string    `json:"http_method,omitempty"`
	HTTPStatus  int       `json:"http_status,omitempty"`
}

// ========================================
// 2. Formatter 인터페이스 (Strategy 패턴)
// ========================================

// OutputFormat은 출력 형식을 나타냅니다.
// 실제 Hubble: pkg/printer/printer.go의 Output 타입
type OutputFormat int

const (
	FormatCompact OutputFormat = iota
	FormatJSON
	FormatDict
	FormatTab
)

func (f OutputFormat) String() string {
	switch f {
	case FormatCompact:
		return "compact"
	case FormatJSON:
		return "json"
	case FormatDict:
		return "dict"
	case FormatTab:
		return "tab"
	default:
		return "unknown"
	}
}

// Printer는 Flow를 지정된 형식으로 출력합니다.
// 실제 Hubble: Printer 구조체 (Functional Options 패턴)
type Printer struct {
	format      OutputFormat
	writer      io.Writer
	jsonEncoder *json.Encoder
	tabWriter   *tabwriter.Writer
	color       bool
	lineNum     int
}

// Option은 Printer 설정 함수입니다.
type Option func(*Printer)

func WithFormat(f OutputFormat) Option {
	return func(p *Printer) { p.format = f }
}

func WithColor(c bool) Option {
	return func(p *Printer) { p.color = c }
}

func WithWriter(w io.Writer) Option {
	return func(p *Printer) { p.writer = w }
}

func NewPrinter(opts ...Option) *Printer {
	p := &Printer{
		format: FormatCompact,
		writer: os.Stdout,
		color:  false,
	}

	for _, opt := range opts {
		opt(p)
	}

	switch p.format {
	case FormatJSON:
		p.jsonEncoder = json.NewEncoder(p.writer)
	case FormatTab:
		p.tabWriter = tabwriter.NewWriter(p.writer, 2, 0, 3, ' ', 0)
	}

	return p
}

// PrintFlow는 Flow를 설정된 형식으로 출력합니다.
func (p *Printer) PrintFlow(flow Flow) {
	p.lineNum++

	switch p.format {
	case FormatCompact:
		p.printCompact(flow)
	case FormatJSON:
		p.printJSON(flow)
	case FormatDict:
		p.printDict(flow)
	case FormatTab:
		p.printTab(flow)
	}
}

// Flush는 버퍼링된 출력을 플러시합니다.
func (p *Printer) Flush() {
	if p.tabWriter != nil {
		p.tabWriter.Flush()
	}
}

// ── 형식별 구현 ──

func (p *Printer) printCompact(flow Flow) {
	ts := flow.Timestamp.Format("Jan 02 15:04:05.000")

	verdict := flow.Verdict
	if p.color {
		switch flow.Verdict {
		case "FORWARDED":
			verdict = "\033[32m" + verdict + "\033[0m" // 초록색
		case "DROPPED":
			verdict = "\033[31m" + verdict + "\033[0m" // 빨간색
		}
	}

	line := fmt.Sprintf("%s: %s -> %s %s %s:%d (%s)",
		ts, flow.Source, flow.Destination,
		verdict, flow.Protocol, flow.Port, flow.Namespace)

	if flow.L7Type != "" {
		line += fmt.Sprintf(" %s %s %d", flow.L7Type, flow.HTTPMethod, flow.HTTPStatus)
	}

	fmt.Fprintln(p.writer, line)
}

func (p *Printer) printJSON(flow Flow) {
	p.jsonEncoder.Encode(flow)
}

func (p *Printer) printDict(flow Flow) {
	fmt.Fprintln(p.writer, "  ---")
	fmt.Fprintf(p.writer, "  timestamp: %s\n", flow.Timestamp.Format(time.RFC3339Nano))
	fmt.Fprintf(p.writer, "  source: %s\n", flow.Source)
	fmt.Fprintf(p.writer, "  destination: %s\n", flow.Destination)
	fmt.Fprintf(p.writer, "  verdict: %s\n", flow.Verdict)
	fmt.Fprintf(p.writer, "  protocol: %s\n", flow.Protocol)
	fmt.Fprintf(p.writer, "  port: %d\n", flow.Port)
	fmt.Fprintf(p.writer, "  namespace: %s\n", flow.Namespace)
	if flow.L7Type != "" {
		fmt.Fprintf(p.writer, "  l7_type: %s\n", flow.L7Type)
		fmt.Fprintf(p.writer, "  http_method: %s\n", flow.HTTPMethod)
		fmt.Fprintf(p.writer, "  http_status: %d\n", flow.HTTPStatus)
	}
}

func (p *Printer) printTab(flow Flow) {
	if p.lineNum == 1 {
		fmt.Fprintln(p.tabWriter, "TIMESTAMP\tSOURCE\tDESTINATION\tVERDICT\tPROTOCOL\tPORT")
		fmt.Fprintln(p.tabWriter, "---------\t------\t-----------\t-------\t--------\t----")
	}
	fmt.Fprintf(p.tabWriter, "%s\t%s\t%s\t%s\t%s\t%d\n",
		flow.Timestamp.Format("15:04:05"),
		flow.Source, flow.Destination,
		flow.Verdict, flow.Protocol, flow.Port)
}

// ========================================
// 3. 실행
// ========================================

func main() {
	fmt.Println("=== PoC: Hubble 출력 포맷터 전략 패턴 ===")
	fmt.Println()
	fmt.Println("hubble observe 출력 형식:")
	fmt.Println("  --output compact : 한 줄 요약 (기본값)")
	fmt.Println("  --output json    : JSON (파이프라인용)")
	fmt.Println("  --output dict    : 키-값 사전 (상세)")
	fmt.Println("  --output tab     : 탭 정렬 테이블")
	fmt.Println()

	now := time.Now()
	flows := []Flow{
		{Timestamp: now, Source: "default/frontend-abc12", Destination: "default/backend-xyz89", Verdict: "FORWARDED", Protocol: "TCP", Port: 8080, Namespace: "default"},
		{Timestamp: now.Add(100 * time.Millisecond), Source: "default/frontend-abc12", Destination: "kube-system/coredns", Verdict: "FORWARDED", Protocol: "UDP", Port: 53, Namespace: "default"},
		{Timestamp: now.Add(200 * time.Millisecond), Source: "untrusted/scanner", Destination: "default/database", Verdict: "DROPPED", Protocol: "TCP", Port: 3306, Namespace: "untrusted"},
		{Timestamp: now.Add(300 * time.Millisecond), Source: "default/frontend-abc12", Destination: "default/api-gateway", Verdict: "FORWARDED", Protocol: "HTTP", Port: 80, Namespace: "default", L7Type: "HTTP", HTTPMethod: "GET", HTTPStatus: 200},
	}

	formats := []struct {
		name   string
		format OutputFormat
		cmd    string
	}{
		{"Compact (기본)", FormatCompact, "hubble observe"},
		{"JSON", FormatJSON, "hubble observe -o json"},
		{"Dict", FormatDict, "hubble observe -o dict"},
		{"Tab", FormatTab, "hubble observe -o tab"},
	}

	for _, f := range formats {
		fmt.Printf("━━━ %s 형식: %s ━━━\n", f.name, f.cmd)
		fmt.Println()

		var buf strings.Builder
		printer := NewPrinter(
			WithFormat(f.format),
			WithWriter(&buf),
		)

		for _, flow := range flows {
			printer.PrintFlow(flow)
		}
		printer.Flush()

		// 들여쓰기 추가
		for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
			fmt.Printf("  %s\n", line)
		}
		fmt.Println()
	}

	// JSON 파이프라인 활용 예시
	fmt.Println("━━━ JSON 활용: jq 파이프라인 ━━━")
	fmt.Println()
	fmt.Println("  # DROPPED만 필터링")
	fmt.Println("  hubble observe -o json | jq 'select(.verdict==\"DROPPED\")'")
	fmt.Println()
	fmt.Println("  # 소스별 카운트")
	fmt.Println("  hubble observe -o json | jq -r .source | sort | uniq -c")
	fmt.Println()

	fmt.Println("핵심 포인트:")
	fmt.Println("  - Strategy 패턴: 같은 데이터, 다른 출력 형식")
	fmt.Println("  - Functional Options: NewPrinter(WithFormat(...), WithColor(...))")
	fmt.Println("  - json.Encoder: 스트리밍 JSON 출력 (메모리 효율)")
	fmt.Println("  - tabwriter: 탭 정렬 테이블 출력")
	fmt.Println("  - JSON 출력은 jq와 파이프라인 조합에 최적화")
}
