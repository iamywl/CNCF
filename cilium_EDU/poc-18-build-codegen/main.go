package main

import (
	"bytes"
	"fmt"
	"go/format"
	"sort"
	"strings"
	"text/template"
)

// =============================================================================
// Cilium Build & Code Generation 파이프라인 시뮬레이션
// =============================================================================
// 실제 소스: tools/dpgen/ (main.go, config.go, maps.go, util.go)
//           tools/crdlistgen/main.go, tools/api-flaggen/main.go
//           pkg/k8s/apis/cilium.io/v2/zz_generated.deepcopy.go
//
// Cilium의 코드 생성 도구 체계:
//
// 1. dpgen: eBPF 오브젝트 → Go 코드 생성
//    - config 서브커맨드: eBPF 변수 → Go 설정 struct (varsToStruct)
//    - maps 서브커맨드: eBPF MapSpec → Go MapSpec 생성자 (renderMapSpecs)
//    - camelCase: snake_case → CamelCase 변환 (stylized 약어 처리)
//    - btfVarGoType: BTF 타입 → Go 타입 변환
//    - text/template + go:embed로 .tpl 파일 사용
//
// 2. deepcopy-gen: CRD struct → DeepCopy/DeepCopyInto 메서드
//    - zz_generated.deepcopy.go 파일 생성
//    - 슬라이스, 맵, 포인터 필드를 재귀적으로 깊은 복사
//
// 3. crdlistgen: CRD 목록 → rst 문서 생성
//    - Documentation 디렉토리를 WalkDir로 순회
//    - RST 파일에서 CRD 이름 매칭 → 링크 생성
//
// 4. api-flaggen: API 스펙 → 플래그 테이블 생성
//    - OpenAPI 스펙에서 경로/설명 추출
//    - tabwriter로 정렬된 테이블 출력
//
// 실행: go run main.go
// =============================================================================

// ============================================================================
// camelCase 변환 (실제: tools/dpgen/config.go)
// ============================================================================

// stylized는 Go 스타일 약어 매핑이다.
// 실제 구현(config.go)에서 BPF, IPv4, MAC 등을 대문자 약어로 변환.
var stylized = map[string]string{
	"bpf":     "BPF",
	"lxc":     "LXC",
	"xdp":     "XDP",
	"ipv4":    "IPv4",
	"ipv6":    "IPv6",
	"nat":     "NAT",
	"mac":     "MAC",
	"mtu":     "MTU",
	"id":      "ID",
	"ip":      "IP",
	"ipcache": "IPCache",
	"icmp":    "ICMP",
	"lb":      "LB",
	"arp":     "ARP",
	"ep":      "EP",
	"fib":     "FIB",
	"ifindex": "IfIndex",
}

// camelCase는 snake_case 문자열을 CamelCase로 변환한다.
// 실제: tools/dpgen/config.go:camelCase
// "foo_bar" → "FooBar", "bpf_ipv4_nat" → "BPFIPv4NAT"
func camelCase(s string) string {
	var b strings.Builder
	for _, w := range strings.Split(s, "_") {
		w = strings.ToLower(w)
		if styled, ok := stylized[w]; ok {
			b.WriteString(styled)
		} else {
			if len(w) > 0 {
				b.WriteString(strings.ToUpper(w[:1]) + w[1:])
			}
		}
	}
	return b.String()
}

// ============================================================================
// btfVarGoType 시뮬레이션 (실제: tools/dpgen/config.go)
// ============================================================================

// BTFEncoding은 BTF 정수 인코딩이다.
type BTFEncoding int

const (
	BTFSigned   BTFEncoding = iota
	BTFUnsigned
	BTFBool
	BTFChar
)

// BTFType은 BTF 타입 정보를 시뮬레이션한다.
// 실제: btf.Int (Encoding, Size 필드)
type BTFType struct {
	Encoding BTFEncoding
	Size     uint32 // 바이트 단위
}

// btfVarGoType는 BTF 변수 타입을 Go 타입 이름으로 변환한다.
// 실제: tools/dpgen/config.go:btfVarGoType
func btfVarGoType(t BTFType) (string, error) {
	switch t.Encoding {
	case BTFChar:
		return "byte", nil
	case BTFBool:
		return "bool", nil
	case BTFSigned:
		if t.Size > 8 {
			return "", fmt.Errorf("unsupported signed size %d", t.Size)
		}
		return fmt.Sprintf("int%d", t.Size*8), nil
	case BTFUnsigned:
		if t.Size > 8 {
			return "", fmt.Errorf("unsupported unsigned size %d", t.Size)
		}
		return fmt.Sprintf("uint%d", t.Size*8), nil
	default:
		return "", fmt.Errorf("unsupported encoding %d", t.Encoding)
	}
}

// ============================================================================
// MapSpec 코드 생성 (실제: tools/dpgen/maps.go)
// ============================================================================

// MapType은 BPF 맵 타입이다.
type MapType string

const (
	MapTypeHash     MapType = "Hash"
	MapTypeLRUHash  MapType = "LRUHash"
	MapTypeArray    MapType = "Array"
	MapTypeLPMTrie  MapType = "LPMTrie"
	MapTypeHashOfMaps MapType = "HashOfMaps"
)

// PinType은 맵 핀 타입이다.
type PinType string

const (
	PinNone     PinType = "PinNone"
	PinByName   PinType = "PinByName"
)

// MapSpec은 eBPF 맵 스펙이다.
// 실제: ebpf.MapSpec (Name, Type, KeySize, ValueSize, MaxEntries, Flags, Pinning)
type MapSpec struct {
	Name       string
	Type       MapType
	KeySize    uint32
	KeyType    string // BTF 키 타입 이름
	ValueSize  uint32
	ValueType  string // BTF 값 타입 이름
	MaxEntries uint32
	Flags      uint32
	Pinning    PinType
	InnerMap   *MapSpec // 맵 of 맵에서 사용
}

// needMapSpec은 MapSpec이 생성 대상인지 판단한다.
// 실제: tools/dpgen/maps.go:needMapSpec - Pinning != PinNone인 것만 출력
func needMapSpec(spec *MapSpec) bool {
	return spec.Pinning != PinNone
}

// mapSpecCompatible은 같은 이름의 MapSpec이 호환되는지 확인한다.
// 실제: tools/dpgen/util.go:mapSpecCompatible
func mapSpecCompatible(a, b *MapSpec) error {
	if a.Type != b.Type {
		return fmt.Errorf("map %s: type mismatch: %s != %s", a.Name, a.Type, b.Type)
	}
	if a.KeySize != b.KeySize {
		return fmt.Errorf("map %s: key size mismatch: %d != %d", a.Name, a.KeySize, b.KeySize)
	}
	if a.ValueSize != b.ValueSize {
		return fmt.Errorf("map %s: value size mismatch: %d != %d", a.Name, a.ValueSize, b.ValueSize)
	}
	if a.MaxEntries != b.MaxEntries {
		return fmt.Errorf("map %s: max entries mismatch: %d != %d", a.Name, a.MaxEntries, b.MaxEntries)
	}
	return nil
}

// bpfFlagsToString은 BPF 맵 플래그를 문자열로 변환한다.
// 실제: tools/dpgen/util.go:bpfFlagsToString
func bpfFlagsToString(flags uint32) string {
	flagNames := map[uint32]string{
		0x01: "BPF_F_NO_PREALLOC",
		0x02: "BPF_F_NO_COMMON_LRU",
		0x80: "BPF_F_RDONLY_PROG",
	}

	var consts []string
	for i := 0; i < 32; i++ {
		flag := flags & (1 << i)
		if flag != 0 {
			if name, ok := flagNames[flag]; ok {
				consts = append(consts, name)
			} else {
				consts = append(consts, fmt.Sprintf("0x%x", flag))
			}
		}
	}

	if len(consts) == 0 {
		return "0"
	}
	return strings.Join(consts, " | ")
}

// mapSpecTpl은 MapSpec Go 코드를 생성하는 템플릿이다.
// 실제: tools/dpgen/maps_generated.go.tpl (go:embed로 로드)
const mapSpecTpl = `// Code generated by dpgen. DO NOT EDIT.

package {{.Package}}

// LoadMapSpecs는 미리 정의된 MapSpec을 반환한다.
func LoadMapSpecs() map[string]*MapSpec {
	out := make(map[string]*MapSpec)
	for _, f := range _outer {
		spec := f()
		out[spec.Name] = spec
	}
	return out
}

type newMapFn func() *MapSpec

var _outer = []newMapFn{
{{- range .OuterMaps}}
	new{{camelCase .Name}}Spec,
{{- end}}
}
{{range .AllMaps}}
func new{{camelCase .Name}}Spec() *MapSpec {
	return &MapSpec{
		Name:       "{{.Name}}",
		Type:       "{{.Type}}",
		KeySize:    {{.KeySize}},
		ValueSize:  {{.ValueSize}},
		MaxEntries: {{.MaxEntries}},
		Flags:      {{bpfFlagsToString .Flags}},
		Pinning:    "{{.Pinning}}",
	}
}
{{end}}`

// renderMapSpecs는 MapSpec Go 코드를 렌더링한다.
// 실제: tools/dpgen/maps.go:renderMapSpecs
func renderMapSpecs(outer, inner map[string]*MapSpec, pkg string) (string, error) {
	tpl, err := template.New("mapSpec").
		Funcs(template.FuncMap{
			"camelCase":        camelCase,
			"bpfFlagsToString": bpfFlagsToString,
		}).
		Parse(mapSpecTpl)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}

	// outer + inner 결합 (실제: maps.Clone + maps.Copy)
	all := make(map[string]*MapSpec)
	for k, v := range outer {
		all[k] = v
	}
	for k, v := range inner {
		all[k] = v
	}

	// 이름 순 정렬 (실제: slices.SortedFunc로 정렬)
	outerSorted := sortMapSpecs(outer)
	allSorted := sortMapSpecs(all)

	var buf bytes.Buffer
	data := struct {
		Package   string
		OuterMaps []*MapSpec
		AllMaps   []*MapSpec
	}{pkg, outerSorted, allSorted}

	if err := tpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}

	return buf.String(), nil
}

func sortMapSpecs(m map[string]*MapSpec) []*MapSpec {
	specs := make([]*MapSpec, 0, len(m))
	for _, s := range m {
		specs = append(specs, s)
	}
	sort.Slice(specs, func(i, j int) bool {
		return specs[i].Name < specs[j].Name
	})
	return specs
}

// ============================================================================
// Config struct 생성 (실제: tools/dpgen/config.go:varsToStruct)
// ============================================================================

// ConfigVar는 eBPF 설정 변수를 시뮬레이션한다.
// 실제: ebpf.VariableSpec (SectionName, Type(btf.Var), Value)
type ConfigVar struct {
	CName   string     // C 변수 이름 (snake_case)
	Type    BTFType    // BTF 타입 정보
	Tags    []string   // 태그 (첫 번째는 doc comment)
	Section string     // ELF 섹션 이름
	Kind    string     // "object" 또는 "node"
}

// ConfigField는 생성될 Go struct 필드이다.
// 실제: tools/dpgen/config.go 내부 field struct
type ConfigField struct {
	Comment  string
	GoName   string
	CName    string
	GoType   string
	DefValue string
}

// varsToStruct는 eBPF 변수에서 Go 설정 struct를 생성한다.
// 실제: tools/dpgen/config.go:varsToStruct
func varsToStruct(vars []ConfigVar, name, kind, comment string, embeds []string) (string, error) {
	targetKind := "kind:" + kind

	var fields []ConfigField
	for _, v := range vars {
		// 지정된 섹션의 변수만 (실제: config.Section 확인)
		if v.Section != ".data.config" {
			continue
		}

		// kind 태그 확인 (실제: slices.Contains(tags, kind))
		hasKind := false
		var filteredTags []string
		for _, tag := range v.Tags {
			if tag == targetKind {
				hasKind = true
			} else {
				filteredTags = append(filteredTags, tag)
			}
		}
		if !hasKind {
			continue
		}

		if len(filteredTags) == 0 || filteredTags[0] == "" {
			return "", fmt.Errorf("variable %s has no doc comment", v.CName)
		}

		// BTF → Go 타입 변환 (실제: btfVarGoType)
		goType, err := btfVarGoType(v.Type)
		if err != nil {
			return "", fmt.Errorf("variable %s: %w", v.CName, err)
		}

		// 태그를 코멘트로 변환 (실제: tagsToComment)
		commentStr := tagsToComment(filteredTags)

		// DECLARE_CONFIG 접두사 제거 (실제: strings.TrimPrefix(n, config.ConstantPrefix))
		cName := strings.TrimPrefix(v.CName, "cilium_cfg_")

		fields = append(fields, ConfigField{
			Comment:  commentStr,
			GoName:   camelCase(cName),
			CName:    cName,
			GoType:   goType,
			DefValue: "0",
		})
	}

	// 이름 순 정렬 (실제: slices.SortStableFunc)
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].GoName < fields[j].GoName
	})

	// struct 렌더링
	var b strings.Builder

	// doc comment (실제: wrapString)
	if comment != "" {
		b.WriteString(fmt.Sprintf("// %s\n", comment))
	}
	b.WriteString(fmt.Sprintf("type %s struct {\n", name))

	for _, f := range fields {
		b.WriteString(f.Comment)
		b.WriteString(fmt.Sprintf("\t%s %s `bpf:\"%s\"`\n", f.GoName, f.GoType, f.CName))
	}

	// 임베드 (실제: embeds 파라미터)
	if len(embeds) > 0 {
		if len(fields) > 0 {
			b.WriteString("\n")
		}
		for _, e := range embeds {
			b.WriteString(fmt.Sprintf("\t%s\n", e))
		}
	}
	b.WriteString("}\n")

	// 생성자 함수 (실제: New{Name} 함수)
	var params []string
	for _, e := range embeds {
		params = append(params, fmt.Sprintf("%s %s", strings.ToLower(e), e))
	}
	b.WriteString(fmt.Sprintf("\nfunc New%s(%s) *%s {\n", name, strings.Join(params, ", "), name))
	b.WriteString(fmt.Sprintf("\treturn &%s{}\n", name))
	b.WriteString("}\n")

	return b.String(), nil
}

// tagsToComment은 태그 슬라이스를 코멘트로 변환한다.
// 실제: tools/dpgen/config.go:tagsToComment
func tagsToComment(tags []string) string {
	var b strings.Builder
	for i, tag := range tags {
		if tag == "" {
			continue
		}
		// 첫 글자 대문자 + 마지막에 마침표 (실제: sentencify)
		tag = sentencify(tag)
		if i > 0 {
			b.WriteString("\t//\n")
		}
		b.WriteString(fmt.Sprintf("\t// %s\n", tag))
	}
	return b.String()
}

// sentencify는 첫 글자를 대문자로, 끝에 마침표를 추가한다.
// 실제: tools/dpgen/config.go:sentencify
func sentencify(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if s[len(s)-1] != '.' {
		s += "."
	}
	return s
}

// ============================================================================
// DeepCopy 생성 (실제: zz_generated.deepcopy.go)
// ============================================================================

// FieldKind는 필드의 종류이다.
type FieldKind int

const (
	FieldScalar    FieldKind = iota // int, string, bool 등 값 타입
	FieldPointer                     // *T 포인터
	FieldSlice                       // []T 슬라이스
	FieldMap                         // map[K]V 맵
	FieldDeepCopy                    // DeepCopyInto 메서드를 가진 타입
)

// StructField는 struct 필드 정보이다.
type StructField struct {
	Name     string
	TypeName string
	Kind     FieldKind
	ElemType string // 슬라이스/맵의 요소 타입
}

// StructDef는 struct 정의이다.
type StructDef struct {
	Name   string
	Fields []StructField
}

// deepCopyTpl은 DeepCopy 코드 생성 템플릿이다.
// 실제 deepcopy-gen이 생성하는 패턴을 재현:
// - DeepCopyInto: *out = *in 으로 시작, 참조 타입만 별도 복사
// - DeepCopy: new(T) 후 DeepCopyInto 호출
const deepCopyTpl = `//go:build !ignore_autogenerated

// Code generated by deepcopy-gen. DO NOT EDIT.

package {{.Package}}
{{range .Structs}}
// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *{{.Name}}) DeepCopyInto(out *{{.Name}}) {
	*out = *in
{{- range .Fields}}
{{- if eq .Kind 1}}
	if in.{{.Name}} != nil {
		in, out := &in.{{.Name}}, &out.{{.Name}}
		*out = new({{.ElemType}})
		**out = **in
	}
{{- else if eq .Kind 2}}
	if in.{{.Name}} != nil {
		in, out := &in.{{.Name}}, &out.{{.Name}}
		*out = make({{.TypeName}}, len(*in))
		copy(*out, *in)
	}
{{- else if eq .Kind 3}}
	if in.{{.Name}} != nil {
		in, out := &in.{{.Name}}, &out.{{.Name}}
		*out = make({{.TypeName}}, len(*in))
		for key, val := range *in {
			(*out)[key] = val
		}
	}
{{- else if eq .Kind 4}}
	if in.{{.Name}} != nil {
		in, out := &in.{{.Name}}, &out.{{.Name}}
		*out = new({{.ElemType}})
		(*in).DeepCopyInto(*out)
	}
{{- end}}
{{- end}}
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new {{.Name}}.
func (in *{{.Name}}) DeepCopy() *{{.Name}} {
	if in == nil {
		return nil
	}
	out := new({{.Name}})
	in.DeepCopyInto(out)
	return out
}
{{end}}`

func renderDeepCopy(pkg string, structs []StructDef) (string, error) {
	tpl, err := template.New("deepcopy").Parse(deepCopyTpl)
	if err != nil {
		return "", fmt.Errorf("parsing deep copy template: %w", err)
	}

	var buf bytes.Buffer
	data := struct {
		Package string
		Structs []StructDef
	}{pkg, structs}

	if err := tpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing deep copy template: %w", err)
	}

	return buf.String(), nil
}

// ============================================================================
// CRD List 생성 시뮬레이션 (실제: tools/crdlistgen/main.go)
// ============================================================================

// CRDEntry는 CRD 정의이다.
type CRDEntry struct {
	Name    string
	Version string
	HasDoc  bool
}

// cleanupCRDName은 CRD 이름에서 버전 부분을 제거한다.
// 실제: tools/crdlistgen/main.go:cleanupCRDName
func cleanupCRDName(name string) string {
	return strings.Split(name, "/")[0]
}

// generateCRDList는 CRD 목록을 RST 형식으로 생성한다.
// 실제: tools/crdlistgen/main.go:printCRDList
// Documentation 디렉토리를 WalkDir로 순회하여 RST 파일에서 CRD 이름 매칭
func generateCRDList(crds []CRDEntry) string {
	// 이름 순 정렬 (실제: slices.Sort)
	names := make([]string, len(crds))
	crdMap := make(map[string]CRDEntry)
	for i, crd := range crds {
		cleaned := cleanupCRDName(crd.Name)
		names[i] = cleaned
		crdMap[cleaned] = crd
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		crd := crdMap[name]
		if crd.HasDoc {
			// RST 참조 링크 (실제: ":ref:`name<name>`")
			b.WriteString(fmt.Sprintf("- :ref:`%s<%s>`\n", name, name))
		} else {
			b.WriteString(fmt.Sprintf("- %s\n", name))
		}
	}
	return b.String()
}

// ============================================================================
// API Flag Table 생성 시뮬레이션 (실제: tools/api-flaggen/main.go)
// ============================================================================

// APIPathEntry는 API 경로 엔트리이다.
type APIPathEntry struct {
	Path        string
	Description string
}

// generateAPIFlagTable은 API 플래그 테이블을 생성한다.
// 실제: tools/api-flaggen/main.go:writeTable (tabwriter 사용)
func generateAPIFlagTable(title, binary, flag string, paths []APIPathEntry) string {
	var b strings.Builder

	// 타이틀 (실제: writeTitle)
	b.WriteString(fmt.Sprintf("\n%s\n", title))
	b.WriteString(strings.Repeat("=", len(title)))
	b.WriteString("\n\n")

	// 프리앰블 (실제: writeFlagPreamble)
	b.WriteString(fmt.Sprintf("The following API flags are compatible with the ``%s`` flag\n``%s``.\n\n", binary, flag))

	// 경로 순 정렬 (실제: slices.Sorted(maps.Keys(pathSet)))
	sort.Slice(paths, func(i, j int) bool {
		return paths[i].Path < paths[j].Path
	})

	// 테이블 (실제: tabwriter 사용)
	b.WriteString(fmt.Sprintf("%-30s %s\n", "Flag Name", "Description"))
	b.WriteString(fmt.Sprintf("%-30s %s\n", strings.Repeat("=", 25), strings.Repeat("=", 40)))
	for _, p := range paths {
		// 실제: wrap() 함수로 설명 줄바꿈
		b.WriteString(fmt.Sprintf("%-30s %s\n", p.Path, p.Description))
	}
	b.WriteString(fmt.Sprintf("%-30s %s\n", strings.Repeat("=", 25), strings.Repeat("=", 40)))

	return b.String()
}

// ============================================================================
// 시뮬레이션 실행
// ============================================================================

func main() {
	fmt.Println("=== Cilium Build & Code Generation 파이프라인 시뮬레이션 ===")
	fmt.Println("소스: tools/dpgen/, tools/crdlistgen/, tools/api-flaggen/")
	fmt.Println()

	// ─── 1. camelCase 변환 ────────────────────────────────────
	fmt.Println("[1] camelCase 변환 (tools/dpgen/config.go)")
	fmt.Println("  snake_case → CamelCase (stylized 약어 처리)")
	fmt.Println()

	testCases := []string{
		"endpoint_id",
		"bpf_ipv4_nat",
		"lxc_mac_addr",
		"xdp_mtu_config",
		"ipcache_lb_entry",
		"tunnel_ep_ifindex",
		"arp_fib_lookup",
	}

	fmt.Printf("  %-25s → %s\n", "입력 (snake_case)", "출력 (CamelCase)")
	fmt.Printf("  %s\n", strings.Repeat("-", 50))
	for _, tc := range testCases {
		fmt.Printf("  %-25s → %s\n", tc, camelCase(tc))
	}
	fmt.Println()

	// ─── 2. btfVarGoType 변환 ─────────────────────────────────
	fmt.Println("[2] btfVarGoType 변환 (BTF → Go 타입)")
	fmt.Println("  실제: tools/dpgen/config.go:btfVarGoType")
	fmt.Println()

	btfTests := []struct {
		name string
		typ  BTFType
	}{
		{"signed 1byte", BTFType{BTFSigned, 1}},
		{"signed 4byte", BTFType{BTFSigned, 4}},
		{"unsigned 2byte", BTFType{BTFUnsigned, 2}},
		{"unsigned 4byte", BTFType{BTFUnsigned, 4}},
		{"unsigned 8byte", BTFType{BTFUnsigned, 8}},
		{"bool", BTFType{BTFBool, 1}},
		{"char", BTFType{BTFChar, 1}},
	}

	for _, bt := range btfTests {
		goType, err := btfVarGoType(bt.typ)
		if err != nil {
			fmt.Printf("  %-20s → 오류: %v\n", bt.name, err)
		} else {
			fmt.Printf("  %-20s → %s\n", bt.name, goType)
		}
	}
	fmt.Println()

	// ─── 3. varsToStruct (Config struct 생성) ─────────────────
	fmt.Println("[3] varsToStruct (eBPF 변수 → Go 설정 struct)")
	fmt.Println("  실제: tools/dpgen/config.go:varsToStruct")
	fmt.Println("  사용: go:generate dpgen config --path bpf_lxc.o --kind node --name Node")
	fmt.Println(strings.Repeat("-", 60))

	configVars := []ConfigVar{
		{
			CName:   "cilium_cfg_endpoint_id",
			Type:    BTFType{BTFUnsigned, 2},
			Tags:    []string{"kind:node", "endpoint identifier used in BPF programs"},
			Section: ".data.config",
			Kind:    "node",
		},
		{
			CName:   "cilium_cfg_ipv4_addr",
			Type:    BTFType{BTFUnsigned, 4},
			Tags:    []string{"kind:node", "IPv4 address of the node"},
			Section: ".data.config",
			Kind:    "node",
		},
		{
			CName:   "cilium_cfg_mtu",
			Type:    BTFType{BTFUnsigned, 2},
			Tags:    []string{"kind:node", "maximum transmission unit for the node"},
			Section: ".data.config",
			Kind:    "node",
		},
		{
			CName:   "cilium_cfg_encrypt_key",
			Type:    BTFType{BTFUnsigned, 1},
			Tags:    []string{"kind:node", "encryption key index"},
			Section: ".data.config",
			Kind:    "node",
		},
		{
			CName:   "cilium_cfg_nat_ipv4_enable",
			Type:    BTFType{BTFBool, 1},
			Tags:    []string{"kind:object", "enable IPv4 NAT for this object"},
			Section: ".data.config",
			Kind:    "object",
		},
	}

	configCode, err := varsToStruct(configVars, "Node", "node",
		"Node is a configuration struct for a Cilium datapath node. Warning: do not instantiate directly! Always use [NewNode].",
		nil)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Println(configCode)
	}
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()

	// ─── 4. MapSpec 코드 생성 ─────────────────────────────────
	fmt.Println("[4] MapSpec 코드 생성 (eBPF 맵 → Go 코드)")
	fmt.Println("  실제: tools/dpgen/maps.go:renderMapSpecs")
	fmt.Println("  사용: go:generate dpgen maps ../../../bpf/bpf_*.o")
	fmt.Println(strings.Repeat("-", 60))

	// eBPF 오브젝트에서 추출된 MapSpec 시뮬레이션
	allMaps := map[string]*MapSpec{
		"cilium_ipcache": {
			Name: "cilium_ipcache", Type: MapTypeLPMTrie,
			KeySize: 24, KeyType: "ipcache_key",
			ValueSize: 32, ValueType: "remote_endpoint_info",
			MaxEntries: 512000, Flags: 0x01, // BPF_F_NO_PREALLOC
			Pinning: PinByName,
		},
		"cilium_lxc": {
			Name: "cilium_lxc", Type: MapTypeHash,
			KeySize: 8, KeyType: "endpoint_key",
			ValueSize: 64, ValueType: "endpoint_info",
			MaxEntries: 65536, Flags: 0,
			Pinning: PinByName,
		},
		"cilium_policy": {
			Name: "cilium_policy", Type: MapTypeHashOfMaps,
			KeySize: 8, KeyType: "policy_key",
			ValueSize: 4, ValueType: "__u32",
			MaxEntries: 65536, Flags: 0,
			Pinning: PinByName,
			InnerMap: &MapSpec{
				Name: "cilium_policy_inner", Type: MapTypeHash,
				KeySize: 32, ValueSize: 8,
				MaxEntries: 16384, Flags: 0x01,
				Pinning: PinNone,
			},
		},
		"cilium_signals": {
			Name: "cilium_signals", Type: MapTypeArray,
			KeySize: 4, ValueSize: 4,
			MaxEntries: 1, Flags: 0,
			Pinning: PinNone, // PinNone → needMapSpec = false
		},
	}

	// outer/inner 분류 (실제: runMaps에서 수행)
	outer := make(map[string]*MapSpec)
	inner := make(map[string]*MapSpec)

	for _, spec := range allMaps {
		if !needMapSpec(spec) {
			fmt.Printf("  SKIP (PinNone): %s\n", spec.Name)
			continue
		}
		outer[spec.Name] = spec
		fmt.Printf("  INCLUDE: %s (Type=%s, Flags=%s)\n", spec.Name, spec.Type, bpfFlagsToString(spec.Flags))

		if spec.InnerMap != nil {
			inner[spec.InnerMap.Name] = spec.InnerMap
			fmt.Printf("  INNER:   %s (Type=%s)\n", spec.InnerMap.Name, spec.InnerMap.Type)
		}
	}
	fmt.Println()

	mapsCode, err := renderMapSpecs(outer, inner, "maps")
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Println(mapsCode)
	}
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()

	// ─── 5. MapSpec 호환성 검사 ──────────────────────────────
	fmt.Println("[5] MapSpec 호환성 검사")
	fmt.Println("  실제: tools/dpgen/util.go:mapSpecCompatible")
	fmt.Println("  여러 BPF 오브젝트에서 같은 맵이 다르게 정의되면 에러")
	fmt.Println()

	specA := &MapSpec{Name: "cilium_lxc", Type: MapTypeHash, KeySize: 8, ValueSize: 64, MaxEntries: 65536}
	specB := &MapSpec{Name: "cilium_lxc", Type: MapTypeHash, KeySize: 8, ValueSize: 64, MaxEntries: 65536}
	specC := &MapSpec{Name: "cilium_lxc", Type: MapTypeHash, KeySize: 8, ValueSize: 128, MaxEntries: 65536}

	if err := mapSpecCompatible(specA, specB); err != nil {
		fmt.Printf("  A vs B: 비호환 - %v\n", err)
	} else {
		fmt.Println("  A vs B: 호환 OK (동일한 Type/KeySize/ValueSize/MaxEntries)")
	}

	if err := mapSpecCompatible(specA, specC); err != nil {
		fmt.Printf("  A vs C: 비호환 - %v\n", err)
	} else {
		fmt.Println("  A vs C: 호환 OK")
	}
	fmt.Println()

	// ─── 6. DeepCopy 생성 ─────────────────────────────────────
	fmt.Println("[6] DeepCopy 코드 생성 (deepcopy-gen)")
	fmt.Println("  실제: pkg/k8s/apis/cilium.io/v2/zz_generated.deepcopy.go")
	fmt.Println("  패턴: DeepCopyInto(*out = *in) + 참조 타입만 별도 복사")
	fmt.Println(strings.Repeat("-", 60))

	structs := []StructDef{
		{
			Name: "CiliumNetworkPolicy",
			Fields: []StructField{
				{Name: "Name", TypeName: "string", Kind: FieldScalar},
				{Name: "Namespace", TypeName: "string", Kind: FieldScalar},
				{Name: "Labels", TypeName: "map[string]string", Kind: FieldMap},
				{Name: "Spec", TypeName: "*CiliumNetworkPolicySpec", Kind: FieldPointer, ElemType: "CiliumNetworkPolicySpec"},
				{Name: "Specs", TypeName: "[]CiliumNetworkPolicySpec", Kind: FieldSlice},
			},
		},
		{
			Name: "CiliumEndpoint",
			Fields: []StructField{
				{Name: "Name", TypeName: "string", Kind: FieldScalar},
				{Name: "Identity", TypeName: "int64", Kind: FieldScalar},
				{Name: "Networking", TypeName: "*EndpointNetworking", Kind: FieldDeepCopy, ElemType: "EndpointNetworking"},
			},
		},
	}

	dcCode, err := renderDeepCopy("v2", structs)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Println(dcCode)
	}
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()

	// ─── 7. CRD List 생성 ─────────────────────────────────────
	fmt.Println("[7] CRD List 생성 (tools/crdlistgen)")
	fmt.Println("  실제: Documentation/crdlist.rst 생성")
	fmt.Println("  WalkDir로 .rst 파일 순회 → CRD 이름 매칭 → ref 링크")
	fmt.Println()

	crds := []CRDEntry{
		{Name: "ciliumnetworkpolicies.cilium.io/v2", Version: "v2", HasDoc: true},
		{Name: "ciliumendpoints.cilium.io/v2", Version: "v2", HasDoc: true},
		{Name: "ciliumidentities.cilium.io/v2", Version: "v2", HasDoc: false},
		{Name: "ciliumnodes.cilium.io/v2", Version: "v2", HasDoc: true},
		{Name: "ciliumexternalworkloads.cilium.io/v2", Version: "v2", HasDoc: false},
		{Name: "ciliumclusterwidenetworkpolicies.cilium.io/v2", Version: "v2", HasDoc: true},
	}

	rstOutput := generateCRDList(crds)
	fmt.Println(rstOutput)

	// ─── 8. API Flag Table 생성 ──────────────────────────────
	fmt.Println("[8] API Flag Table 생성 (tools/api-flaggen)")
	fmt.Println("  실제: 문서 자동 생성 (RST 테이블)")

	paths := []APIPathEntry{
		{Path: "/v1/endpoint", Description: "List all endpoints"},
		{Path: "/v1/endpoint/{id}", Description: "Get endpoint by ID"},
		{Path: "/v1/policy", Description: "Manage network policies"},
		{Path: "/v1/identity", Description: "List security identities"},
		{Path: "/v1/service", Description: "Manage load-balanced services"},
		{Path: "/v1/prefilter", Description: "Manage CIDR-based prefiltering"},
	}

	table := generateAPIFlagTable("Cilium Agent API", "cilium-agent",
		"enable-cilium-api-server-access", paths)
	fmt.Println(table)

	// ─── 9. go/format을 이용한 코드 포맷팅 ────────────────────
	fmt.Println("[9] go/format.Source 코드 포맷팅")
	fmt.Println("  실제: dpgen에서 Go 코드 생성 후 gofmt 적용")
	fmt.Println()

	unformatted := `package main

import "fmt"

func   main(  ) {
fmt.Println(   "hello"   )
var   x    int  =   42
_ = x
}`

	fmt.Println("  포맷 전:")
	for _, line := range strings.Split(unformatted, "\n") {
		fmt.Printf("    %s\n", line)
	}

	formatted, err := format.Source([]byte(unformatted))
	if err != nil {
		fmt.Printf("  포맷 오류: %v\n", err)
	} else {
		fmt.Println()
		fmt.Println("  포맷 후:")
		for _, line := range strings.Split(string(formatted), "\n") {
			fmt.Printf("    %s\n", line)
		}
	}
	fmt.Println()

	// ─── 구조 요약 ───────────────────────────────────────────
	fmt.Println("=== Cilium 코드 생성 도구 체계 ===")
	fmt.Println()
	fmt.Println("  tools/dpgen")
	fmt.Println("  ├── config 서브커맨드")
	fmt.Println("  │   ├── eBPF 오브젝트 로드 (LoadCollectionSpec)")
	fmt.Println("  │   ├── Variables 순회 → .data.config 섹션만")
	fmt.Println("  │   ├── btfVarGoType: BTF Int → Go int/uint/bool")
	fmt.Println("  │   ├── camelCase: snake_case → CamelCase (stylized)")
	fmt.Println("  │   ├── varsToStruct: 필드 수집 → struct 렌더링")
	fmt.Println("  │   └── 출력: Node, BPFLXC, BPFXDP 등 설정 struct")
	fmt.Println("  │")
	fmt.Println("  ├── maps 서브커맨드")
	fmt.Println("  │   ├── glob 패턴으로 .o 파일 매칭")
	fmt.Println("  │   ├── MapSpec 추출 (Pinned 맵만)")
	fmt.Println("  │   ├── mapSpecCompatible: 오브젝트 간 호환성 검증")
	fmt.Println("  │   ├── BTF 키/값 타입 수집 → combined BTF blob")
	fmt.Println("  │   ├── renderMapSpecs: text/template → Go 코드")
	fmt.Println("  │   └── 출력: maps_generated.go + mapkv.btf")
	fmt.Println("  │")
	fmt.Println("  └── go:generate 연동")
	fmt.Println("      ├── pkg/datapath/config/gen.go (config)")
	fmt.Println("      └── pkg/datapath/maps/gen.go (maps)")
	fmt.Println()
	fmt.Println("  tools/crdlistgen")
	fmt.Println("  └── CRD 목록 → Documentation/crdlist.rst")
	fmt.Println()
	fmt.Println("  tools/api-flaggen")
	fmt.Println("  └── OpenAPI 스펙 → API 플래그 테이블 (RST)")
	fmt.Println()
	fmt.Println("  deepcopy-gen (외부 도구)")
	fmt.Println("  └── CRD struct → zz_generated.deepcopy.go")
	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}
