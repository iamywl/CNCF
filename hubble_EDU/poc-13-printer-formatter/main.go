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

// =============================================================================
// Hubble 프린터/포매터 시뮬레이션
//
// 실제 구현 참조:
//   hubble/pkg/printer/printer.go  - Printer, WriteProtoFlow()
//   hubble/pkg/printer/options.go  - Output enum, Options, Option 함수
//   hubble/pkg/printer/color.go    - colorer, verdict별 색상
//
// 핵심 개념:
//   1. 다중 출력 포맷: Compact, Dict, JSON, Tab (tabwriter)
//   2. ANSI 색상 코딩: verdict별 색상 (forwarded=green, dropped=red 등)
//   3. Printer 옵션: Writer, TimeFormat, NodeName, IPTranslation
//   4. tabwriter: 탭 정렬된 테이블 출력
// =============================================================================

// --- 색상 시스템 ---
// 실제: printer/color.go - colorer

const (
	// ANSI 색상 코드
	colorReset   = "\033[0m"
	colorRed     = "\033[31m"
	colorGreen   = "\033[32m"
	colorYellow  = "\033[33m"
	colorBlue    = "\033[34m"
	colorMagenta = "\033[35m"
	colorCyan    = "\033[36m"
)

type Colorer struct {
	enabled bool
}

func NewColorer(when string) *Colorer {
	c := &Colorer{}
	switch strings.ToLower(when) {
	case "always":
		c.enabled = true
	case "never":
		c.enabled = false
	case "auto":
		// 실제: color.NoColor 전역 변수로 터미널 감지
		// PoC에서는 stdout이 터미널인지 간단히 판단
		if fileInfo, _ := os.Stdout.Stat(); fileInfo != nil {
			c.enabled = (fileInfo.Mode() & os.ModeCharDevice) != 0
		}
	}
	return c
}

func (c *Colorer) wrap(color, text string) string {
	if !c.enabled {
		return text
	}
	return color + text + colorReset
}

// 실제: colorer.verdictForwarded()
func (c *Colorer) VerdictForwarded(s string) string { return c.wrap(colorGreen, s) }

// 실제: colorer.verdictDropped()
func (c *Colorer) VerdictDropped(s string) string { return c.wrap(colorRed, s) }

// 실제: colorer.verdictAudit()
func (c *Colorer) VerdictAudit(s string) string { return c.wrap(colorYellow, s) }

// 실제: colorer.verdictTraced()
func (c *Colorer) VerdictTraced(s string) string { return c.wrap(colorYellow, s) }

// 실제: colorer.verdictTranslated()
func (c *Colorer) VerdictTranslated(s string) string { return c.wrap(colorYellow, s) }

// 실제: colorer.host()
func (c *Colorer) Host(s string) string { return c.wrap(colorCyan, s) }

// 실제: colorer.identity()
func (c *Colorer) Identity(s string) string { return c.wrap(colorMagenta, s) }

// 실제: colorer.port()
func (c *Colorer) Port(s string) string { return c.wrap(colorYellow, s) }

// --- 출력 포맷 ---
// 실제: printer/options.go - Output enum

type OutputFormat int

const (
	TabOutput     OutputFormat = iota // tabwriter 기반 테이블
	CompactOutput                     // 한 줄 요약
	DictOutput                        // KEY: VALUE 사전형
	JSONOutput                        // JSON (proto3 매핑)
)

func (o OutputFormat) String() string {
	switch o {
	case TabOutput:
		return "table"
	case CompactOutput:
		return "compact"
	case DictOutput:
		return "dict"
	case JSONOutput:
		return "json"
	default:
		return "unknown"
	}
}

// --- Flow 데이터 모델 ---

type Verdict string

const (
	VerdictForwarded   Verdict = "FORWARDED"
	VerdictDropped     Verdict = "DROPPED"
	VerdictAudit       Verdict = "AUDIT"
	VerdictRedirected  Verdict = "REDIRECTED"
	VerdictTraced      Verdict = "TRACED"
	VerdictTranslated  Verdict = "TRANSLATED"
)

type Endpoint struct {
	Namespace string `json:"namespace"`
	PodName   string `json:"pod_name"`
	Identity  uint32 `json:"identity"`
}

type Flow struct {
	Time        time.Time `json:"time"`
	NodeName    string    `json:"node_name"`
	Source      *Endpoint `json:"source"`
	Destination *Endpoint `json:"destination"`
	SrcIP       string    `json:"src_ip"`
	DstIP       string    `json:"dst_ip"`
	SrcPort     int       `json:"src_port"`
	DstPort     int       `json:"dst_port"`
	Verdict     Verdict   `json:"verdict"`
	Type        string    `json:"type"`
	Summary     string    `json:"summary"`
	IsReply     *bool     `json:"is_reply"`
}

// GetFlowsResponse는 서버 응답
type GetFlowsResponse struct {
	Flow     *Flow  `json:"flow"`
	Time     string `json:"time"`
	NodeName string `json:"node_name"`
}

// --- Printer 옵션 ---
// 실제: printer/options.go

type PrinterOptions struct {
	output              OutputFormat
	w                   io.Writer
	werr                io.Writer
	timeFormat          string
	nodeName            bool
	enableIPTranslation bool
	color               string
}

type PrinterOption func(*PrinterOptions)

func WithOutput(output OutputFormat) PrinterOption {
	return func(o *PrinterOptions) { o.output = output }
}

func WithWriter(w io.Writer) PrinterOption {
	return func(o *PrinterOptions) { o.w = w }
}

func WithTimeFormat(layout string) PrinterOption {
	return func(o *PrinterOptions) { o.timeFormat = layout }
}

func WithNodeName() PrinterOption {
	return func(o *PrinterOptions) { o.nodeName = true }
}

func WithIPTranslation() PrinterOption {
	return func(o *PrinterOptions) { o.enableIPTranslation = true }
}

func WithColor(when string) PrinterOption {
	return func(o *PrinterOptions) { o.color = when }
}

// --- Printer ---
// 실제: printer/printer.go

type Printer struct {
	opts        PrinterOptions
	line        int
	tw          *tabwriter.Writer
	jsonEncoder *json.Encoder
	color       *Colorer
}

func NewPrinter(opts ...PrinterOption) *Printer {
	// 기본 옵션
	// 실제: printer.New()의 기본값
	options := PrinterOptions{
		output:     TabOutput,
		w:          os.Stdout,
		werr:       os.Stderr,
		timeFormat: time.StampMilli,
		color:      "auto",
	}

	for _, opt := range opts {
		opt(&options)
	}

	p := &Printer{
		opts:  options,
		color: NewColorer(options.color),
	}

	switch options.output {
	case TabOutput:
		// tabwriter는 색상과 호환되지 않으므로 비활성화
		// 실제: p.color.disable()
		p.tw = tabwriter.NewWriter(options.w, 2, 0, 3, ' ', 0)
		p.color.enabled = false
	case JSONOutput:
		p.jsonEncoder = json.NewEncoder(options.w)
	}

	return p
}

// Close는 tabwriter flush 등 정리 작업
// 실제: Printer.Close()
func (p *Printer) Close() error {
	if p.tw != nil {
		return p.tw.Flush()
	}
	return nil
}

// GetHostName은 IP 주소를 호스트명으로 변환
// 실제: Printer.Hostname()
func (p *Printer) GetHostName(ip string, port int, ns, pod, svc string) string {
	host := ip
	if p.opts.enableIPTranslation {
		if pod != "" {
			if ns != "" {
				host = ns + "/" + pod
			} else {
				host = pod
			}
		} else if svc != "" {
			if ns != "" {
				host = ns + "/" + svc
			} else {
				host = svc
			}
		}
	}

	if port > 0 {
		return fmt.Sprintf("%s:%s", p.color.Host(host), p.color.Port(fmt.Sprintf("%d", port)))
	}
	return p.color.Host(host)
}

// fmtIdentity는 보안 ID를 포맷팅
// 실제: Printer.fmtIdentity()
func (p *Printer) fmtIdentity(id uint32) string {
	// 실제에서는 reserved identity 체크 후 이름 표시
	if id < 256 {
		return p.color.Identity(fmt.Sprintf("(reserved:%d)", id))
	}
	return p.color.Identity(fmt.Sprintf("(ID:%d)", id))
}

// getVerdict는 verdict를 색상이 적용된 문자열로 반환
// 실제: Printer.getVerdict()
func (p *Printer) getVerdict(f *Flow) string {
	switch f.Verdict {
	case VerdictForwarded, VerdictRedirected:
		return p.color.VerdictForwarded(string(f.Verdict))
	case VerdictDropped:
		return p.color.VerdictDropped(string(f.Verdict))
	case VerdictAudit:
		return p.color.VerdictAudit(string(f.Verdict))
	case VerdictTraced:
		return p.color.VerdictTraced(string(f.Verdict))
	case VerdictTranslated:
		return p.color.VerdictTranslated(string(f.Verdict))
	default:
		return string(f.Verdict)
	}
}

// WriteProtoFlow는 Flow를 지정된 포맷으로 출력
// 실제: Printer.WriteProtoFlow()
func (p *Printer) WriteProtoFlow(res *GetFlowsResponse) error {
	f := res.Flow

	// 호스트명 생성
	srcHost := p.GetHostName(f.SrcIP, f.SrcPort, f.Source.Namespace, f.Source.PodName, "")
	dstHost := p.GetHostName(f.DstIP, f.DstPort, f.Destination.Namespace, f.Destination.PodName, "")
	srcId := p.fmtIdentity(f.Source.Identity)
	dstId := p.fmtIdentity(f.Destination.Identity)

	switch p.opts.output {
	case TabOutput:
		// 실제: tabwriter 기반 테이블 출력
		if p.line == 0 {
			headers := []string{"TIMESTAMP"}
			if p.opts.nodeName {
				headers = append(headers, "NODE")
			}
			headers = append(headers, "SOURCE", "DESTINATION", "TYPE", "VERDICT", "SUMMARY")
			fmt.Fprintln(p.tw, strings.Join(headers, "\t"))
		}
		row := []string{f.Time.Format(p.opts.timeFormat)}
		if p.opts.nodeName {
			row = append(row, f.NodeName)
		}
		row = append(row, srcHost, dstHost, f.Type, string(f.Verdict), f.Summary)
		fmt.Fprintln(p.tw, strings.Join(row, "\t"))

	case CompactOutput:
		// 실제: compact 모드 - 한 줄 요약
		var node string
		if p.opts.nodeName {
			node = fmt.Sprintf(" [%s]", f.NodeName)
		}

		// 화살표 방향: reply면 반전
		arrow := "->"
		src, dst := srcHost, dstHost
		srcIdentity, dstIdentity := srcId, dstId
		if f.IsReply != nil && *f.IsReply {
			src, dst = dstHost, srcHost
			srcIdentity, dstIdentity = dstId, srcId
			arrow = "<-"
		} else if f.IsReply == nil {
			arrow = "<>"
		}

		fmt.Fprintf(p.opts.w, "%s%s: %s %s %s %s %s %s %s (%s)\n",
			f.Time.Format(p.opts.timeFormat),
			node,
			src, srcIdentity,
			arrow,
			dst, dstIdentity,
			f.Type,
			p.getVerdict(f),
			f.Summary,
		)

	case DictOutput:
		// 실제: dict 모드 - KEY: VALUE
		if p.line > 0 {
			fmt.Fprintln(p.opts.w, "  "+strings.Repeat("-", 40))
		}
		fmt.Fprintf(p.opts.w, "    TIMESTAMP: %s\n", f.Time.Format(p.opts.timeFormat))
		if p.opts.nodeName {
			fmt.Fprintf(p.opts.w, "         NODE: %s\n", f.NodeName)
		}
		fmt.Fprintf(p.opts.w, "       SOURCE: %s\n", srcHost)
		fmt.Fprintf(p.opts.w, "  DESTINATION: %s\n", dstHost)
		fmt.Fprintf(p.opts.w, "         TYPE: %s\n", f.Type)
		fmt.Fprintf(p.opts.w, "      VERDICT: %s\n", p.getVerdict(f))
		fmt.Fprintf(p.opts.w, "      SUMMARY: %s\n", f.Summary)

	case JSONOutput:
		// 실제: jsonpb 모드 - JSON 직렬화
		return p.jsonEncoder.Encode(res)
	}

	p.line++
	return nil
}

// --- 테스트 데이터 ---

func sampleFlows() []*GetFlowsResponse {
	boolTrue := true
	boolFalse := false

	return []*GetFlowsResponse{
		{
			Flow: &Flow{
				Time: time.Now().Add(-5 * time.Second), NodeName: "k8s-node-0",
				Source:      &Endpoint{Namespace: "default", PodName: "frontend-7b4d8c-abc12", Identity: 12345},
				Destination: &Endpoint{Namespace: "default", PodName: "backend-5f6a7b-def34", Identity: 67890},
				SrcIP: "10.0.0.1", DstIP: "10.0.0.2", SrcPort: 42356, DstPort: 8080,
				Verdict: VerdictForwarded, Type: "to-endpoint", Summary: "TCP Flags: SYN",
				IsReply: &boolFalse,
			},
			NodeName: "k8s-node-0",
		},
		{
			Flow: &Flow{
				Time: time.Now().Add(-4 * time.Second), NodeName: "k8s-node-0",
				Source:      &Endpoint{Namespace: "default", PodName: "backend-5f6a7b-def34", Identity: 67890},
				Destination: &Endpoint{Namespace: "default", PodName: "frontend-7b4d8c-abc12", Identity: 12345},
				SrcIP: "10.0.0.2", DstIP: "10.0.0.1", SrcPort: 8080, DstPort: 42356,
				Verdict: VerdictForwarded, Type: "to-endpoint", Summary: "TCP Flags: SYN, ACK",
				IsReply: &boolTrue,
			},
			NodeName: "k8s-node-0",
		},
		{
			Flow: &Flow{
				Time: time.Now().Add(-3 * time.Second), NodeName: "k8s-node-1",
				Source:      &Endpoint{Namespace: "prod", PodName: "api-gateway-1a2b3c-ghi56", Identity: 11111},
				Destination: &Endpoint{Namespace: "kube-system", PodName: "coredns-6d4b5f-xyz99", Identity: 2},
				SrcIP: "10.0.1.5", DstIP: "10.96.0.10", SrcPort: 55123, DstPort: 53,
				Verdict: VerdictForwarded, Type: "L7/dns-request", Summary: "Query api.internal.svc A",
				IsReply: &boolFalse,
			},
			NodeName: "k8s-node-1",
		},
		{
			Flow: &Flow{
				Time: time.Now().Add(-2 * time.Second), NodeName: "k8s-node-2",
				Source:      &Endpoint{Namespace: "monitoring", PodName: "prometheus-0", Identity: 22222},
				Destination: &Endpoint{Namespace: "default", PodName: "backend-5f6a7b-def34", Identity: 67890},
				SrcIP: "10.0.2.10", DstIP: "10.0.0.2", SrcPort: 38901, DstPort: 9090,
				Verdict: VerdictDropped, Type: "policy-verdict:none INGRESS", Summary: "TCP Flags: SYN; Policy denied",
				IsReply: &boolFalse,
			},
			NodeName: "k8s-node-2",
		},
		{
			Flow: &Flow{
				Time: time.Now().Add(-1 * time.Second), NodeName: "k8s-node-0",
				Source:      &Endpoint{Namespace: "default", PodName: "frontend-7b4d8c-abc12", Identity: 12345},
				Destination: &Endpoint{Namespace: "default", PodName: "backend-5f6a7b-def34", Identity: 67890},
				SrcIP: "10.0.0.1", DstIP: "10.0.0.2", SrcPort: 42356, DstPort: 8080,
				Verdict: VerdictAudit, Type: "policy-verdict:L4Only EGRESS", Summary: "TCP Flags: SYN; Audited",
				IsReply: nil, // direction unknown
			},
			NodeName: "k8s-node-0",
		},
	}
}

func main() {
	fmt.Println("=== Hubble 프린터/포매터 시뮬레이션 ===")
	fmt.Println()
	fmt.Println("참조: hubble/pkg/printer/printer.go - Printer.WriteProtoFlow()")
	fmt.Println("참조: hubble/pkg/printer/options.go - Output enum, Options")
	fmt.Println("참조: hubble/pkg/printer/color.go   - colorer, verdict별 색상")
	fmt.Println()

	flows := sampleFlows()

	// === 포맷 1: Compact (기본) ===
	fmt.Println("=== 1. Compact 출력 (hubble observe -o compact) ===")
	fmt.Println()
	p1 := NewPrinter(
		WithOutput(CompactOutput),
		WithIPTranslation(),
		WithColor("always"),
	)
	for _, f := range flows {
		p1.WriteProtoFlow(f)
	}
	p1.Close()
	fmt.Println()

	// === 포맷 2: Compact + NodeName ===
	fmt.Println("=== 2. Compact + 노드명 (hubble observe -o compact --print-node-name) ===")
	fmt.Println()
	p2 := NewPrinter(
		WithOutput(CompactOutput),
		WithIPTranslation(),
		WithNodeName(),
		WithColor("always"),
	)
	for _, f := range flows {
		p2.WriteProtoFlow(f)
	}
	p2.Close()
	fmt.Println()

	// === 포맷 3: Table ===
	fmt.Println("=== 3. Table 출력 (hubble observe -o table) ===")
	fmt.Println()
	p3 := NewPrinter(
		WithOutput(TabOutput),
		WithIPTranslation(),
	)
	for _, f := range flows {
		p3.WriteProtoFlow(f)
	}
	p3.Close()
	fmt.Println()

	// === 포맷 4: Table + NodeName ===
	fmt.Println("=== 4. Table + 노드명 (hubble observe -o table --print-node-name) ===")
	fmt.Println()
	p4 := NewPrinter(
		WithOutput(TabOutput),
		WithIPTranslation(),
		WithNodeName(),
	)
	for _, f := range flows {
		p4.WriteProtoFlow(f)
	}
	p4.Close()
	fmt.Println()

	// === 포맷 5: Dict ===
	fmt.Println("=== 5. Dict 출력 (hubble observe -o dict) ===")
	fmt.Println()
	p5 := NewPrinter(
		WithOutput(DictOutput),
		WithIPTranslation(),
		WithNodeName(),
		WithColor("always"),
	)
	for _, f := range flows[:2] { // 2개만 출력
		p5.WriteProtoFlow(f)
	}
	p5.Close()
	fmt.Println()

	// === 포맷 6: JSON ===
	fmt.Println("=== 6. JSON 출력 (hubble observe -o json) ===")
	fmt.Println()
	p6 := NewPrinter(
		WithOutput(JSONOutput),
	)
	for _, f := range flows[:2] { // 2개만 출력
		p6.WriteProtoFlow(f)
	}
	p6.Close()
	fmt.Println()

	// === 포맷 7: IP 주소 직접 출력 (IP 변환 비활성화) ===
	fmt.Println("=== 7. Compact (IP 변환 없음) ===")
	fmt.Println()
	p7 := NewPrinter(
		WithOutput(CompactOutput),
		WithColor("always"),
	)
	for _, f := range flows[:2] {
		p7.WriteProtoFlow(f)
	}
	p7.Close()
	fmt.Println()

	// === 포맷 8: 색상 비활성화 ===
	fmt.Println("=== 8. Compact (색상 없음, --color never) ===")
	fmt.Println()
	p8 := NewPrinter(
		WithOutput(CompactOutput),
		WithIPTranslation(),
		WithColor("never"),
	)
	for _, f := range flows[:3] {
		p8.WriteProtoFlow(f)
	}
	p8.Close()
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
	fmt.Println()
	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. 4가지 출력 포맷: compact, table, dict, json")
	fmt.Println("  2. Compact: 한 줄 요약, reply면 화살표/src/dst 반전")
	fmt.Println("  3. Table: tabwriter로 탭 정렬 (색상 비활성화)")
	fmt.Println("  4. Dict: KEY: VALUE, 구분선으로 이벤트 분리")
	fmt.Println("  5. ANSI 색상: FORWARDED=green, DROPPED=red, AUDIT=yellow")
	fmt.Println("  6. Functional Options: WithOutput(), WithColor() 등")
}
