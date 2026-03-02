// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble Field Mask 최적화 패턴
//
// Field Mask는 클라이언트가 필요한 필드만 요청하는 패턴입니다:
//   - 네트워크 대역폭 절약 (불필요한 필드 전송 안 함)
//   - 서버 CPU 절약 (불필요한 필드 계산 안 함)
//   - Protobuf의 google.protobuf.FieldMask 표준 사용
//
// 실제 Hubble: GetFlows 요청 시 fieldmask로 필요한 필드만 요청
//
// 실행: go run main.go

package main

import (
	"fmt"
	"strings"
)

// ========================================
// 1. Flow 데이터 구조
// ========================================

type Endpoint struct {
	Namespace string
	PodName   string
	Labels    []string
	Identity  uint32
}

type L4Info struct {
	Protocol  string
	SrcPort   uint16
	DstPort   uint16
}

type L7Info struct {
	Type       string
	HTTPMethod string
	HTTPStatus int
	URL        string
	Headers    map[string]string
}

type Flow struct {
	Source      *Endpoint
	Destination *Endpoint
	Verdict     string
	L4          *L4Info
	L7          *L7Info
	Summary     string
	NodeName    string
	IsReply     bool
}

// FullFlow는 모든 필드가 채워진 Flow를 생성합니다.
func FullFlow() *Flow {
	return &Flow{
		Source: &Endpoint{
			Namespace: "default",
			PodName:   "frontend-abc12",
			Labels:    []string{"app=frontend", "version=v1", "tier=web"},
			Identity:  12345,
		},
		Destination: &Endpoint{
			Namespace: "default",
			PodName:   "backend-xyz89",
			Labels:    []string{"app=backend", "version=v2", "tier=api"},
			Identity:  67890,
		},
		Verdict:  "FORWARDED",
		L4:       &L4Info{Protocol: "TCP", SrcPort: 45678, DstPort: 8080},
		L7:       &L7Info{Type: "HTTP", HTTPMethod: "GET", HTTPStatus: 200, URL: "/api/v1/users", Headers: map[string]string{"Content-Type": "application/json", "Authorization": "Bearer xxx"}},
		Summary:  "HTTP/2 GET /api/v1/users → 200 OK",
		NodeName: "k8s-node-1",
		IsReply:  false,
	}
}

// ========================================
// 2. Field Mask 구현 (Hubble의 fieldmask 패턴)
// ========================================

// FieldMask는 트리 구조로 필드 경로를 저장합니다.
// 실제 Hubble: pkg/hubble/parser/fieldmask/fieldmask.go
//
// 예시: ["source.namespace", "source.pod_name", "verdict"]
// →  { "source": { "namespace": nil, "pod_name": nil }, "verdict": nil }
type FieldMask map[string]FieldMask

// Parse는 필드 경로 목록을 FieldMask 트리로 변환합니다.
func Parse(paths []string) FieldMask {
	fm := make(FieldMask)
	for _, path := range paths {
		fm.add(path)
	}
	return fm
}

// add는 단일 경로를 트리에 추가합니다.
// "source.namespace" → { "source": { "namespace": nil } }
func (fm FieldMask) add(path string) {
	prefix, suffix, found := strings.Cut(path, ".")
	if !found {
		// 리프 노드
		fm[prefix] = nil
		return
	}
	// 중간 노드
	if child, ok := fm[prefix]; !ok || child == nil {
		fm[prefix] = make(FieldMask)
	}
	fm[prefix].add(suffix)
}

// Contains는 주어진 필드가 마스크에 포함되는지 확인합니다.
func (fm FieldMask) Contains(path string) bool {
	if fm == nil {
		return false
	}
	prefix, suffix, found := strings.Cut(path, ".")
	child, ok := fm[prefix]
	if !ok {
		return false
	}
	if child == nil || !found {
		return true // 리프 노드이거나 전체 서브트리 포함
	}
	return child.Contains(suffix)
}

// String은 FieldMask를 시각적으로 표현합니다.
func (fm FieldMask) String() string {
	return fm.stringIndent(0)
}

func (fm FieldMask) stringIndent(indent int) string {
	var sb strings.Builder
	prefix := strings.Repeat("  ", indent)
	for key, child := range fm {
		if child == nil {
			sb.WriteString(fmt.Sprintf("%s%s (leaf)\n", prefix, key))
		} else {
			sb.WriteString(fmt.Sprintf("%s%s:\n", prefix, key))
			sb.WriteString(child.stringIndent(indent + 1))
		}
	}
	return sb.String()
}

// ========================================
// 3. Selective Copy (필드 마스크에 따른 선택적 복사)
// ========================================

// CopyFlow는 FieldMask에 따라 필요한 필드만 복사합니다.
func CopyFlow(src *Flow, fm FieldMask) *Flow {
	if fm == nil {
		return src // 마스크 없으면 전체 복사
	}

	dst := &Flow{}

	if sub, ok := fm["source"]; ok {
		if src.Source != nil {
			dst.Source = copyEndpoint(src.Source, sub)
		}
	}

	if sub, ok := fm["destination"]; ok {
		if src.Destination != nil {
			dst.Destination = copyEndpoint(src.Destination, sub)
		}
	}

	if _, ok := fm["verdict"]; ok {
		dst.Verdict = src.Verdict
	}

	if sub, ok := fm["l4"]; ok {
		if src.L4 != nil {
			dst.L4 = copyL4(src.L4, sub)
		}
	}

	if sub, ok := fm["l7"]; ok {
		if src.L7 != nil {
			dst.L7 = copyL7(src.L7, sub)
		}
	}

	if _, ok := fm["summary"]; ok {
		dst.Summary = src.Summary
	}

	if _, ok := fm["node_name"]; ok {
		dst.NodeName = src.NodeName
	}

	if _, ok := fm["is_reply"]; ok {
		dst.IsReply = src.IsReply
	}

	return dst
}

func copyEndpoint(src *Endpoint, fm FieldMask) *Endpoint {
	if fm == nil {
		return src // 서브마스크 없으면 전체 복사
	}
	dst := &Endpoint{}
	if _, ok := fm["namespace"]; ok {
		dst.Namespace = src.Namespace
	}
	if _, ok := fm["pod_name"]; ok {
		dst.PodName = src.PodName
	}
	if _, ok := fm["labels"]; ok {
		dst.Labels = src.Labels
	}
	if _, ok := fm["identity"]; ok {
		dst.Identity = src.Identity
	}
	return dst
}

func copyL4(src *L4Info, fm FieldMask) *L4Info {
	if fm == nil {
		return src
	}
	dst := &L4Info{}
	if _, ok := fm["protocol"]; ok {
		dst.Protocol = src.Protocol
	}
	if _, ok := fm["src_port"]; ok {
		dst.SrcPort = src.SrcPort
	}
	if _, ok := fm["dst_port"]; ok {
		dst.DstPort = src.DstPort
	}
	return dst
}

func copyL7(src *L7Info, fm FieldMask) *L7Info {
	if fm == nil {
		return src
	}
	dst := &L7Info{}
	if _, ok := fm["type"]; ok {
		dst.Type = src.Type
	}
	if _, ok := fm["http_method"]; ok {
		dst.HTTPMethod = src.HTTPMethod
	}
	if _, ok := fm["http_status"]; ok {
		dst.HTTPStatus = src.HTTPStatus
	}
	if _, ok := fm["url"]; ok {
		dst.URL = src.URL
	}
	return dst
}

// ========================================
// 4. 출력 헬퍼
// ========================================

func printFlow(prefix string, f *Flow) {
	fmt.Printf("%sFlow:\n", prefix)
	if f.Source != nil {
		fmt.Printf("%s  source:\n", prefix)
		if f.Source.Namespace != "" {
			fmt.Printf("%s    namespace: %s\n", prefix, f.Source.Namespace)
		}
		if f.Source.PodName != "" {
			fmt.Printf("%s    pod_name: %s\n", prefix, f.Source.PodName)
		}
		if len(f.Source.Labels) > 0 {
			fmt.Printf("%s    labels: %v\n", prefix, f.Source.Labels)
		}
		if f.Source.Identity > 0 {
			fmt.Printf("%s    identity: %d\n", prefix, f.Source.Identity)
		}
	}
	if f.Destination != nil {
		fmt.Printf("%s  destination:\n", prefix)
		if f.Destination.Namespace != "" {
			fmt.Printf("%s    namespace: %s\n", prefix, f.Destination.Namespace)
		}
		if f.Destination.PodName != "" {
			fmt.Printf("%s    pod_name: %s\n", prefix, f.Destination.PodName)
		}
		if len(f.Destination.Labels) > 0 {
			fmt.Printf("%s    labels: %v\n", prefix, f.Destination.Labels)
		}
	}
	if f.Verdict != "" {
		fmt.Printf("%s  verdict: %s\n", prefix, f.Verdict)
	}
	if f.L4 != nil {
		fmt.Printf("%s  l4: %s :%d → :%d\n", prefix, f.L4.Protocol, f.L4.SrcPort, f.L4.DstPort)
	}
	if f.L7 != nil {
		fmt.Printf("%s  l7: %s %s %d %s\n", prefix, f.L7.Type, f.L7.HTTPMethod, f.L7.HTTPStatus, f.L7.URL)
	}
	if f.Summary != "" {
		fmt.Printf("%s  summary: %s\n", prefix, f.Summary)
	}
	if f.NodeName != "" {
		fmt.Printf("%s  node_name: %s\n", prefix, f.NodeName)
	}
}

func estimateSize(f *Flow) int {
	size := 0
	if f.Source != nil {
		size += len(f.Source.Namespace) + len(f.Source.PodName) + 4 // identity
		for _, l := range f.Source.Labels {
			size += len(l)
		}
	}
	if f.Destination != nil {
		size += len(f.Destination.Namespace) + len(f.Destination.PodName) + 4
		for _, l := range f.Destination.Labels {
			size += len(l)
		}
	}
	size += len(f.Verdict) + len(f.Summary) + len(f.NodeName) + 1 // IsReply
	if f.L4 != nil {
		size += len(f.L4.Protocol) + 4
	}
	if f.L7 != nil {
		size += len(f.L7.Type) + len(f.L7.HTTPMethod) + 4 + len(f.L7.URL)
		for k, v := range f.L7.Headers {
			size += len(k) + len(v)
		}
	}
	return size
}

// ========================================
// 5. 실행
// ========================================

func main() {
	fmt.Println("=== PoC: Hubble Field Mask 최적화 패턴 ===")
	fmt.Println()
	fmt.Println("Field Mask: 클라이언트가 필요한 필드만 요청")
	fmt.Println("  → 네트워크 대역폭 절약")
	fmt.Println("  → 서버 CPU 절약 (불필요한 계산 스킵)")
	fmt.Println()

	srcFlow := FullFlow()
	fullSize := estimateSize(srcFlow)

	// ── 시나리오 0: 전체 Flow (마스크 없음) ──
	fmt.Println("━━━ 시나리오 0: 전체 Flow (Field Mask 없음) ━━━")
	fmt.Println()
	printFlow("  ", srcFlow)
	fmt.Printf("\n  예상 크기: ~%d bytes\n\n", fullSize)

	// ── 시나리오 1: verdict + source.pod_name만 ──
	fmt.Println("━━━ 시나리오 1: verdict, source.pod_name만 요청 ━━━")
	fmt.Println("  사용 사례: 간단한 모니터링 대시보드")
	fmt.Println()

	fm1 := Parse([]string{"verdict", "source.pod_name", "destination.pod_name"})
	fmt.Println("  Field Mask 트리:")
	for _, line := range strings.Split(strings.TrimRight(fm1.String(), "\n"), "\n") {
		fmt.Printf("    %s\n", line)
	}
	fmt.Println()

	filtered1 := CopyFlow(srcFlow, fm1)
	printFlow("  ", filtered1)
	size1 := estimateSize(filtered1)
	fmt.Printf("\n  예상 크기: ~%d bytes (%.0f%% 절약)\n\n",
		size1, float64(fullSize-size1)/float64(fullSize)*100)

	// ── 시나리오 2: 네트워크 분석용 ──
	fmt.Println("━━━ 시나리오 2: 네트워크 분석용 (L4 + verdict) ━━━")
	fmt.Println("  사용 사례: 네트워크 트래픽 분석")
	fmt.Println()

	fm2 := Parse([]string{
		"source.namespace", "source.pod_name",
		"destination.namespace", "destination.pod_name",
		"verdict", "l4",
	})

	filtered2 := CopyFlow(srcFlow, fm2)
	printFlow("  ", filtered2)
	size2 := estimateSize(filtered2)
	fmt.Printf("\n  예상 크기: ~%d bytes (%.0f%% 절약)\n\n",
		size2, float64(fullSize-size2)/float64(fullSize)*100)

	// ── 시나리오 3: L7 상세 분석 ──
	fmt.Println("━━━ 시나리오 3: L7 HTTP 상세 분석 ━━━")
	fmt.Println("  사용 사례: API 호출 모니터링")
	fmt.Println()

	fm3 := Parse([]string{
		"source.pod_name",
		"destination.pod_name",
		"l7.http_method", "l7.http_status", "l7.url",
	})

	filtered3 := CopyFlow(srcFlow, fm3)
	printFlow("  ", filtered3)
	size3 := estimateSize(filtered3)
	fmt.Printf("\n  예상 크기: ~%d bytes (%.0f%% 절약)\n\n",
		size3, float64(fullSize-size3)/float64(fullSize)*100)

	// ── Contains 테스트 ──
	fmt.Println("━━━ Field Mask Contains 테스트 ━━━")
	fmt.Println()

	fm := Parse([]string{"source.namespace", "source.pod_name", "verdict", "l4.protocol"})
	testPaths := []string{
		"source",           // 중간 노드 → true (하위 필드 있음)
		"source.namespace", // 리프 노드 → true
		"source.labels",    // 없는 필드 → false
		"verdict",          // 리프 노드 → true
		"l7",               // 없는 필드 → false
		"l4.protocol",      // 리프 노드 → true
		"l4.src_port",      // 없는 필드 → false
	}

	for _, path := range testPaths {
		result := fm.Contains(path)
		mark := "✗"
		if result {
			mark = "✓"
		}
		fmt.Printf("  %s fm.Contains(%q) = %t\n", mark, path, result)
	}

	fmt.Println()
	fmt.Println("핵심 포인트:")
	fmt.Println("  - 트리 구조: 점(.)으로 구분된 경로를 재귀적 트리로 변환")
	fmt.Println("  - 선택적 복사: 필요한 필드만 복사하여 메모리/네트워크 절약")
	fmt.Println("  - 실제 Hubble: protoreflect.Message로 동적 필드 복사")
	fmt.Println("  - Protobuf 표준: google.protobuf.FieldMask 메시지 사용")
	fmt.Println("  - 대규모 스트리밍에서 대역폭 50-80% 절약 가능")
}
