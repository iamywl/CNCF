package main

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// =============================================================================
// Prometheus Relabeling Pipeline PoC
// =============================================================================
// 원본: prometheus/model/relabel/relabel.go
//
// Prometheus의 relabeling은 서비스 디스커버리가 생성한 타겟 레이블이나
// 수집된 메트릭의 레이블을 변환하는 파이프라인이다.
// - relabel_configs: 스크랩 전 타겟의 레이블 변환
// - metric_relabel_configs: 스크랩 후 메트릭의 레이블 변환
//
// 핵심 동작:
// 1. SourceLabels 값을 Separator로 연결
// 2. Regex로 매칭
// 3. Action에 따라 변환/필터링 수행
// =============================================================================

// ---------------------------------------------------------------------------
// Label & Builder: 레이블 집합 관리
// ---------------------------------------------------------------------------

// Label은 키-값 쌍이다. Prometheus 레이블은 항상 이름순으로 정렬된다.
type Label struct {
	Name  string
	Value string
}

// Labels는 이름순 정렬된 레이블 슬라이스다.
type Labels []Label

func (ls Labels) Len() int           { return len(ls) }
func (ls Labels) Less(i, j int) bool { return ls[i].Name < ls[j].Name }
func (ls Labels) Swap(i, j int)      { ls[i], ls[j] = ls[j], ls[i] }

// Get은 주어진 이름의 레이블 값을 반환한다. 없으면 빈 문자열.
func (ls Labels) Get(name string) string {
	for _, l := range ls {
		if l.Name == name {
			return l.Value
		}
	}
	return ""
}

func (ls Labels) String() string {
	var sb strings.Builder
	sb.WriteString("{")
	for i, l := range ls {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%s=%q", l.Name, l.Value)
	}
	sb.WriteString("}")
	return sb.String()
}

// Builder는 레이블 집합을 변형하기 위한 빌더다.
// Prometheus 원본(labels.Builder)과 동일한 패턴:
// 기존 레이블을 기반으로 Set/Del 연산을 누적하고, Labels()로 최종 결과를 생성한다.
type Builder struct {
	base Labels
	del  map[string]struct{}
	add  map[string]string
}

// NewBuilder는 기존 레이블 집합으로부터 Builder를 생성한다.
func NewBuilder(base Labels) *Builder {
	return &Builder{
		base: base,
		del:  make(map[string]struct{}),
		add:  make(map[string]string),
	}
}

// Get은 현재 빌더 상태에서 레이블 값을 반환한다.
func (b *Builder) Get(name string) string {
	if v, ok := b.add[name]; ok {
		return v
	}
	if _, ok := b.del[name]; ok {
		return ""
	}
	return b.base.Get(name)
}

// Set은 레이블을 설정한다.
func (b *Builder) Set(name, value string) {
	delete(b.del, name)
	b.add[name] = value
}

// Del은 레이블을 삭제한다.
func (b *Builder) Del(name string) {
	delete(b.add, name)
	b.del[name] = struct{}{}
}

// Range는 현재 상태의 모든 레이블을 순회한다.
// 원본의 lb.Range()와 동일: LabelMap, LabelDrop, LabelKeep에서 사용.
func (b *Builder) Range(fn func(Label)) {
	// 순회 중 수정을 허용하기 위해 스냅샷을 먼저 만든다.
	snapshot := b.Labels()
	for _, l := range snapshot {
		fn(l)
	}
}

// Labels는 최종 레이블 집합을 정렬하여 반환한다.
// 빈 값의 레이블은 제외한다 (Prometheus 동작과 동일).
func (b *Builder) Labels() Labels {
	result := make(Labels, 0, len(b.base)+len(b.add))
	// 기존 레이블 중 삭제되지 않은 것
	for _, l := range b.base {
		if _, deleted := b.del[l.Name]; deleted {
			continue
		}
		if v, overridden := b.add[l.Name]; overridden {
			if v != "" {
				result = append(result, Label{Name: l.Name, Value: v})
			}
		} else {
			result = append(result, l)
		}
	}
	// 신규 추가 레이블 (기존에 없던 것)
	existing := make(map[string]bool, len(b.base))
	for _, l := range b.base {
		existing[l.Name] = true
	}
	for name, value := range b.add {
		if !existing[name] && value != "" {
			result = append(result, Label{Name: name, Value: value})
		}
	}
	sort.Sort(result)
	return result
}

// ---------------------------------------------------------------------------
// Action 타입 & RelabelConfig
// ---------------------------------------------------------------------------

type Action string

const (
	Replace   Action = "replace"
	Keep      Action = "keep"
	Drop      Action = "drop"
	HashMod   Action = "hashmod"
	LabelMap  Action = "labelmap"
	LabelDrop Action = "labeldrop"
	LabelKeep Action = "labelkeep"
	Lowercase Action = "lowercase"
	Uppercase Action = "uppercase"
)

// RelabelConfig는 하나의 relabeling 규칙이다.
// Prometheus 원본의 relabel.Config와 1:1 대응.
type RelabelConfig struct {
	SourceLabels []string // 값을 가져올 레이블 이름들
	Separator    string   // SourceLabels 값 연결 구분자 (기본 ";")
	Regex        *regexp.Regexp // 매칭할 정규식 (기본 "(.*)")
	TargetLabel  string   // 결과를 기록할 레이블
	Replacement  string   // 대체 문자열 (기본 "$1")
	Action       Action   // 수행할 액션
	Modulus      uint64   // hashmod용 모듈러스
}

// DefaultConfig는 기본 설정값을 반환한다.
// 원본: DefaultRelabelConfig — Separator=";", Regex="(.*)", Replacement="$1", Action=Replace
func DefaultConfig() RelabelConfig {
	return RelabelConfig{
		Separator:   ";",
		Regex:       regexp.MustCompile(`^(?s:(.*))$`),
		Replacement: "$1",
		Action:      Replace,
	}
}

// NewConfig는 편의 생성자다. 기본값 위에 커스텀 필드를 덮어쓴다.
func NewConfig(customize func(*RelabelConfig)) *RelabelConfig {
	cfg := DefaultConfig()
	customize(&cfg)
	return &cfg
}

// ---------------------------------------------------------------------------
// Process: 핵심 relabeling 로직
// ---------------------------------------------------------------------------

// Process는 레이블 집합에 relabel 설정들을 순차 적용한다.
// 원본: relabel.Process() → ProcessBuilder() → relabel()
// 반환값이 nil이면 해당 타겟/메트릭이 드롭된 것이다.
func Process(labels Labels, cfgs ...*RelabelConfig) Labels {
	lb := NewBuilder(labels)
	for _, cfg := range cfgs {
		if !applyRelabel(cfg, lb) {
			return nil // 드롭됨
		}
	}
	return lb.Labels()
}

// applyRelabel은 단일 relabel 규칙을 적용한다.
// 원본의 relabel() 함수와 동일한 로직.
func applyRelabel(cfg *RelabelConfig, lb *Builder) bool {
	// 1) SourceLabels 값을 Separator로 연결
	values := make([]string, len(cfg.SourceLabels))
	for i, ln := range cfg.SourceLabels {
		values[i] = lb.Get(ln)
	}
	val := strings.Join(values, cfg.Separator)

	switch cfg.Action {
	case Drop:
		// Regex에 매칭되면 드롭
		if cfg.Regex.MatchString(val) {
			return false
		}

	case Keep:
		// Regex에 매칭되지 않으면 드롭
		if !cfg.Regex.MatchString(val) {
			return false
		}

	case Replace:
		// 원본의 Replace 로직:
		// Regex로 매칭 → ExpandString으로 TargetLabel과 Replacement에 캡처 그룹 치환
		indexes := cfg.Regex.FindStringSubmatchIndex(val)
		if indexes == nil {
			break
		}
		target := string(cfg.Regex.ExpandString([]byte{}, cfg.TargetLabel, val, indexes))
		res := string(cfg.Regex.ExpandString([]byte{}, cfg.Replacement, val, indexes))
		if res == "" {
			lb.Del(target)
		} else {
			lb.Set(target, res)
		}

	case HashMod:
		// 원본: md5 해시의 하위 8바이트를 Modulus로 나눈 나머지
		hash := md5.Sum([]byte(val))
		mod := binary.BigEndian.Uint64(hash[8:]) % cfg.Modulus
		lb.Set(cfg.TargetLabel, strconv.FormatUint(mod, 10))

	case LabelMap:
		// 레이블 이름이 Regex에 매칭되면 → Replacement로 치환한 새 이름으로 복사
		lb.Range(func(l Label) {
			if cfg.Regex.MatchString(l.Name) {
				newName := cfg.Regex.ReplaceAllString(l.Name, cfg.Replacement)
				lb.Set(newName, l.Value)
			}
		})

	case LabelDrop:
		// 레이블 이름이 Regex에 매칭되면 삭제
		lb.Range(func(l Label) {
			if cfg.Regex.MatchString(l.Name) {
				lb.Del(l.Name)
			}
		})

	case LabelKeep:
		// 레이블 이름이 Regex에 매칭되지 않으면 삭제
		lb.Range(func(l Label) {
			if !cfg.Regex.MatchString(l.Name) {
				lb.Del(l.Name)
			}
		})

	case Lowercase:
		lb.Set(cfg.TargetLabel, strings.ToLower(val))

	case Uppercase:
		lb.Set(cfg.TargetLabel, strings.ToUpper(val))

	default:
		panic(fmt.Sprintf("unknown relabel action: %s", cfg.Action))
	}

	return true
}

// ---------------------------------------------------------------------------
// 유틸리티: anchored regex (Prometheus 방식)
// ---------------------------------------------------------------------------

// anchoredRegex는 Prometheus가 사용하는 ^(?s:...)$ 앵커링된 정규식을 생성한다.
// 원본: NewRegexp("s") → regexp.Compile("^(?s:" + s + ")$")
func anchoredRegex(pattern string) *regexp.Regexp {
	return regexp.MustCompile(`^(?s:` + pattern + `)$`)
}

// ---------------------------------------------------------------------------
// 데모 시나리오
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("=== Prometheus Relabeling Pipeline PoC ===")
	fmt.Println()

	scenario1_TargetRelabeling()
	scenario2_KeepDrop()
	scenario3_HashMod()
	scenario4_MetricRelabelConfigs()
	scenario5_LabelMap()
	scenario6_FullPipeline()
}

// ---------------------------------------------------------------------------
// 시나리오 1: 타겟 Relabeling — __meta_kubernetes_* → 유용한 레이블
// ---------------------------------------------------------------------------
// Kubernetes SD가 발견한 타겟에는 __meta_kubernetes_* 레이블이 붙는다.
// relabel_configs로 이 메타 레이블을 사용자 친화적 레이블로 변환한다.
func scenario1_TargetRelabeling() {
	fmt.Println("--- 시나리오 1: 타겟 Relabeling (__meta_kubernetes_* → 유용한 레이블) ---")

	// Kubernetes SD가 발견한 Pod 타겟의 원본 레이블
	discovered := Labels{
		{Name: "__address__", Value: "10.244.0.5:8080"},
		{Name: "__meta_kubernetes_namespace", Value: "production"},
		{Name: "__meta_kubernetes_pod_name", Value: "api-server-7d9f8c6b4-x2k9m"},
		{Name: "__meta_kubernetes_pod_label_app", Value: "api-server"},
		{Name: "__meta_kubernetes_pod_label_version", Value: "v2.1.0"},
		{Name: "__meta_kubernetes_node_name", Value: "worker-03"},
		{Name: "__metrics_path__", Value: "/metrics"},
		{Name: "__scheme__", Value: "http"},
	}
	fmt.Printf("  발견된 레이블: %s\n", discovered)

	// relabel_configs 적용
	configs := []*RelabelConfig{
		// __meta_kubernetes_namespace → namespace
		NewConfig(func(c *RelabelConfig) {
			c.SourceLabels = []string{"__meta_kubernetes_namespace"}
			c.TargetLabel = "namespace"
		}),
		// __meta_kubernetes_pod_name → pod
		NewConfig(func(c *RelabelConfig) {
			c.SourceLabels = []string{"__meta_kubernetes_pod_name"}
			c.TargetLabel = "pod"
		}),
		// __meta_kubernetes_pod_label_app → app
		NewConfig(func(c *RelabelConfig) {
			c.SourceLabels = []string{"__meta_kubernetes_pod_label_app"}
			c.TargetLabel = "app"
		}),
		// __meta_kubernetes_node_name → node
		NewConfig(func(c *RelabelConfig) {
			c.SourceLabels = []string{"__meta_kubernetes_node_name"}
			c.TargetLabel = "node"
		}),
	}

	result := Process(discovered, configs...)
	fmt.Printf("  변환된 레이블: %s\n\n", result)
}

// ---------------------------------------------------------------------------
// 시나리오 2: keep/drop — 레이블 정규식으로 타겟 필터링
// ---------------------------------------------------------------------------
// 실무에서 가장 많이 사용되는 패턴:
// - 특정 네임스페이스만 스크랩 (keep)
// - 테스트 환경 제외 (drop)
func scenario2_KeepDrop() {
	fmt.Println("--- 시나리오 2: keep/drop (타겟 필터링) ---")

	targets := []Labels{
		{
			{Name: "__address__", Value: "10.0.0.1:8080"},
			{Name: "__meta_kubernetes_namespace", Value: "production"},
			{Name: "__meta_kubernetes_pod_label_app", Value: "web-frontend"},
		},
		{
			{Name: "__address__", Value: "10.0.0.2:8080"},
			{Name: "__meta_kubernetes_namespace", Value: "staging"},
			{Name: "__meta_kubernetes_pod_label_app", Value: "web-frontend"},
		},
		{
			{Name: "__address__", Value: "10.0.0.3:8080"},
			{Name: "__meta_kubernetes_namespace", Value: "production"},
			{Name: "__meta_kubernetes_pod_label_app", Value: "test-runner"},
		},
		{
			{Name: "__address__", Value: "10.0.0.4:9090"},
			{Name: "__meta_kubernetes_namespace", Value: "kube-system"},
			{Name: "__meta_kubernetes_pod_label_app", Value: "kube-dns"},
		},
	}

	// keep: production 네임스페이스만 유지
	keepConfig := []*RelabelConfig{
		{
			SourceLabels: []string{"__meta_kubernetes_namespace"},
			Regex:        anchoredRegex("production"),
			Action:       Keep,
		},
	}

	fmt.Println("  [keep: namespace=production만 유지]")
	for _, t := range targets {
		ns := t.Get("__meta_kubernetes_namespace")
		result := Process(t, keepConfig...)
		if result == nil {
			fmt.Printf("    %s (ns=%s) → DROPPED\n", t.Get("__address__"), ns)
		} else {
			fmt.Printf("    %s (ns=%s) → KEPT\n", t.Get("__address__"), ns)
		}
	}
	fmt.Println()

	// drop: test-* 앱 제외
	dropConfig := []*RelabelConfig{
		{
			SourceLabels: []string{"__meta_kubernetes_pod_label_app"},
			Regex:        anchoredRegex("test-.*"),
			Action:       Drop,
		},
	}

	fmt.Println("  [drop: app=test-* 제외]")
	for _, t := range targets {
		app := t.Get("__meta_kubernetes_pod_label_app")
		result := Process(t, dropConfig...)
		if result == nil {
			fmt.Printf("    %s (app=%s) → DROPPED\n", t.Get("__address__"), app)
		} else {
			fmt.Printf("    %s (app=%s) → KEPT\n", t.Get("__address__"), app)
		}
	}
	fmt.Println()
}

// ---------------------------------------------------------------------------
// 시나리오 3: hashmod — Prometheus 인스턴스 간 타겟 샤딩
// ---------------------------------------------------------------------------
// 대규모 환경에서 여러 Prometheus 인스턴스가 타겟을 나눠 스크랩할 때 사용.
// 원본: md5 해시의 하위 8바이트 % modulus
func scenario3_HashMod() {
	fmt.Println("--- 시나리오 3: hashmod (타겟 샤딩) ---")

	// 3개의 Prometheus 인스턴스로 샤딩 (modulus=3)
	numShards := uint64(3)

	targets := Labels{
		{Name: "__address__", Value: "10.0.0.1:8080"},
		{Name: "__address__", Value: "10.0.0.2:8080"},
		{Name: "__address__", Value: "10.0.0.3:8080"},
		{Name: "__address__", Value: "10.0.0.4:8080"},
		{Name: "__address__", Value: "10.0.0.5:8080"},
		{Name: "__address__", Value: "10.0.0.6:8080"},
		{Name: "__address__", Value: "10.0.0.7:8080"},
		{Name: "__address__", Value: "10.0.0.8:8080"},
		{Name: "__address__", Value: "10.0.0.9:8080"},
	}

	// 각 Prometheus 인스턴스(shard 0, 1, 2)가 담당할 타겟 분류
	shardBuckets := make(map[uint64][]string)

	for _, t := range targets {
		// hashmod 적용
		input := Labels{{Name: "__address__", Value: t.Value}}
		configs := []*RelabelConfig{
			{
				SourceLabels: []string{"__address__"},
				TargetLabel:  "__tmp_hash",
				Action:       HashMod,
				Modulus:      numShards,
			},
		}
		result := Process(input, configs...)
		hashVal := result.Get("__tmp_hash")
		shard, _ := strconv.ParseUint(hashVal, 10, 64)
		shardBuckets[shard] = append(shardBuckets[shard], t.Value)
	}

	for shard := uint64(0); shard < numShards; shard++ {
		fmt.Printf("  Prometheus 인스턴스 %d: %v\n", shard, shardBuckets[shard])
	}

	// 실제 사용: hashmod + keep 조합으로 특정 shard만 스크랩
	fmt.Println()
	fmt.Println("  [Prometheus 인스턴스 0이 사용할 설정]")
	fmt.Println("  hashmod(__address__, modulus=3) → __tmp_hash")
	fmt.Println("  keep(__tmp_hash == 0)")
	fmt.Println()
}

// ---------------------------------------------------------------------------
// 시나리오 4: metric_relabel_configs — 불필요한 메트릭 드롭, 레이블 변환
// ---------------------------------------------------------------------------
// 스크랩 후 수집된 메트릭에 적용. 스토리지 비용 절감에 핵심적.
func scenario4_MetricRelabelConfigs() {
	fmt.Println("--- 시나리오 4: metric_relabel_configs (메트릭 후처리) ---")

	// 스크랩으로 수집된 메트릭들
	metrics := []Labels{
		{{Name: "__name__", Value: "http_requests_total"}, {Name: "method", Value: "GET"}, {Name: "status", Value: "200"}},
		{{Name: "__name__", Value: "http_requests_total"}, {Name: "method", Value: "POST"}, {Name: "status", Value: "201"}},
		{{Name: "__name__", Value: "go_gc_duration_seconds"}, {Name: "quantile", Value: "0.5"}},
		{{Name: "__name__", Value: "go_goroutines"}},
		{{Name: "__name__", Value: "http_request_duration_bucket"}, {Name: "METHOD", Value: "GET"}, {Name: "le", Value: "0.1"}},
		{{Name: "__name__", Value: "process_cpu_seconds_total"}},
	}

	configs := []*RelabelConfig{
		// 1) go_* 메트릭 드롭 (스토리지 절감)
		{
			SourceLabels: []string{"__name__"},
			Regex:        anchoredRegex("go_.*"),
			Action:       Drop,
		},
		// 2) METHOD 레이블을 소문자로 변환하여 method에 저장
		//    replace + regex로 비어있지 않은 경우만 변환
		{
			SourceLabels: []string{"METHOD"},
			Regex:        anchoredRegex("(.+)"),
			TargetLabel:  "method",
			Replacement:  "$1",
			Action:       Replace,
		},
		// 3) method 레이블을 소문자화
		{
			SourceLabels: []string{"method"},
			TargetLabel:  "method",
			Action:       Lowercase,
		},
		// 4) 변환 후 원본 METHOD 레이블 제거 (labeldrop)
		{
			Regex:  anchoredRegex("METHOD"),
			Action: LabelDrop,
		},
	}

	fmt.Println("  [규칙: go_* 드롭 + METHOD→method 복사 + 소문자화 + METHOD 제거]")
	for _, m := range metrics {
		result := Process(m, configs...)
		if result == nil {
			fmt.Printf("    %s → DROPPED\n", m.Get("__name__"))
		} else {
			fmt.Printf("    %s → %s\n", m.Get("__name__"), result)
		}
	}
	fmt.Println()
}

// ---------------------------------------------------------------------------
// 시나리오 5: labelmap — __meta_* 레이블을 깔끔한 이름으로 매핑
// ---------------------------------------------------------------------------
// labelmap은 정규식으로 레이블 이름을 매칭하고, 캡처 그룹으로 새 이름을 만든다.
func scenario5_LabelMap() {
	fmt.Println("--- 시나리오 5: labelmap (__meta_* → 클린 레이블) ---")

	discovered := Labels{
		{Name: "__address__", Value: "10.0.1.1:9090"},
		{Name: "__meta_consul_dc", Value: "us-east-1"},
		{Name: "__meta_consul_node", Value: "consul-server-01"},
		{Name: "__meta_consul_service", Value: "payment-api"},
		{Name: "__meta_consul_tag_env", Value: "prod"},
		{Name: "__meta_consul_tag_team", Value: "platform"},
	}
	fmt.Printf("  원본: %s\n", discovered)

	// labelmap: __meta_consul_tag_(.+) → $1
	// Consul SD의 태그 메타 레이블을 일반 레이블로 변환
	configs := []*RelabelConfig{
		{
			Regex:       regexp.MustCompile(`^(?s:__meta_consul_tag_(.+))$`),
			Replacement: "$1",
			Action:      LabelMap,
		},
		// __meta_consul_service → job (일반적인 관례)
		NewConfig(func(c *RelabelConfig) {
			c.SourceLabels = []string{"__meta_consul_service"}
			c.TargetLabel = "job"
		}),
		// __meta_consul_dc → datacenter
		NewConfig(func(c *RelabelConfig) {
			c.SourceLabels = []string{"__meta_consul_dc"}
			c.TargetLabel = "datacenter"
		}),
		// 모든 __meta_ 와 __ 접두사 레이블 제거 (스크랩 전 자동 삭제되는 것 시뮬레이션)
		{
			Regex:  anchoredRegex("__.*"),
			Action: LabelDrop,
		},
	}

	result := Process(discovered, configs...)
	fmt.Printf("  변환: %s\n\n", result)
}

// ---------------------------------------------------------------------------
// 시나리오 6: 전체 파이프라인 — 발견 → relabel → 스크랩 → metric_relabel → 최종
// ---------------------------------------------------------------------------
// 실제 Prometheus 동작 흐름을 모두 시뮬레이션한다.
func scenario6_FullPipeline() {
	fmt.Println("--- 시나리오 6: 전체 파이프라인 (발견 → relabel → metric_relabel → 최종) ---")
	fmt.Println()

	// 1단계: 서비스 디스커버리가 타겟 발견
	discovered := Labels{
		{Name: "__address__", Value: "10.244.2.15:8080"},
		{Name: "__meta_kubernetes_namespace", Value: "production"},
		{Name: "__meta_kubernetes_pod_name", Value: "order-service-5f7d9c8b6-abc12"},
		{Name: "__meta_kubernetes_pod_label_app", Value: "order-service"},
		{Name: "__meta_kubernetes_pod_label_version", Value: "v3.2.1"},
		{Name: "__meta_kubernetes_pod_label_team", Value: "Commerce"},
		{Name: "__meta_kubernetes_pod_annotation_prometheus_io_scrape", Value: "true"},
		{Name: "__meta_kubernetes_pod_annotation_prometheus_io_port", Value: "8080"},
		{Name: "__meta_kubernetes_pod_annotation_prometheus_io_path", Value: "/internal/metrics"},
		{Name: "__metrics_path__", Value: "/metrics"},
		{Name: "__scheme__", Value: "http"},
	}
	fmt.Println("  [1단계] 서비스 디스커버리 결과:")
	printLabels(discovered, "    ")

	// 2단계: relabel_configs 적용
	relabelConfigs := []*RelabelConfig{
		// annotation으로 스크랩 여부 결정
		{
			SourceLabels: []string{"__meta_kubernetes_pod_annotation_prometheus_io_scrape"},
			Regex:        anchoredRegex("true"),
			Action:       Keep,
		},
		// annotation에서 메트릭 경로 추출
		NewConfig(func(c *RelabelConfig) {
			c.SourceLabels = []string{"__meta_kubernetes_pod_annotation_prometheus_io_path"}
			c.TargetLabel = "__metrics_path__"
		}),
		// 유용한 레이블 생성
		NewConfig(func(c *RelabelConfig) {
			c.SourceLabels = []string{"__meta_kubernetes_namespace"}
			c.TargetLabel = "namespace"
		}),
		NewConfig(func(c *RelabelConfig) {
			c.SourceLabels = []string{"__meta_kubernetes_pod_name"}
			c.TargetLabel = "pod"
		}),
		// labelmap: pod_label_(.+) → $1 (모든 Pod 레이블을 Prometheus 레이블로)
		{
			Regex:       regexp.MustCompile(`^(?s:__meta_kubernetes_pod_label_(.+))$`),
			Replacement: "$1",
			Action:      LabelMap,
		},
		// team 레이블 소문자화
		{
			SourceLabels: []string{"team"},
			TargetLabel:  "team",
			Action:       Lowercase,
		},
		// __ 접두사 레이블 제거 (Prometheus가 스크랩 시 자동으로 하는 것)
		{
			Regex:  anchoredRegex("__.*"),
			Action: LabelDrop,
		},
	}

	relabeled := Process(discovered, relabelConfigs...)
	fmt.Println()
	fmt.Println("  [2단계] relabel_configs 적용 후 (타겟 레이블):")
	printLabels(relabeled, "    ")

	// 3단계: 스크랩 시뮬레이션 — 타겟에서 수집된 메트릭들
	fmt.Println()
	fmt.Println("  [3단계] 스크랩된 메트릭 (타겟 레이블 + 메트릭 레이블):")
	scrapedMetrics := []Labels{
		appendLabels(relabeled, Labels{
			{Name: "__name__", Value: "http_requests_total"},
			{Name: "method", Value: "GET"},
			{Name: "status", Value: "200"},
		}),
		appendLabels(relabeled, Labels{
			{Name: "__name__", Value: "http_request_duration_seconds_bucket"},
			{Name: "method", Value: "GET"},
			{Name: "le", Value: "0.25"},
		}),
		appendLabels(relabeled, Labels{
			{Name: "__name__", Value: "go_gc_duration_seconds"},
			{Name: "quantile", Value: "0.75"},
		}),
		appendLabels(relabeled, Labels{
			{Name: "__name__", Value: "process_resident_memory_bytes"},
		}),
	}

	for _, m := range scrapedMetrics {
		fmt.Printf("    %s: %s\n", m.Get("__name__"), m)
	}

	// 4단계: metric_relabel_configs 적용
	metricRelabelConfigs := []*RelabelConfig{
		// go_* 와 process_* 메트릭 드롭
		{
			SourceLabels: []string{"__name__"},
			Regex:        anchoredRegex("(go|process)_.*"),
			Action:       Drop,
		},
		// version 레이블은 메트릭에 불필요 (카디널리티 폭발 방지)
		{
			Regex:  anchoredRegex("version"),
			Action: LabelDrop,
		},
	}

	fmt.Println()
	fmt.Println("  [4단계] metric_relabel_configs 적용 후 (최종 저장):")
	for _, m := range scrapedMetrics {
		result := Process(m, metricRelabelConfigs...)
		if result == nil {
			fmt.Printf("    %s → DROPPED (스토리지 절감)\n", m.Get("__name__"))
		} else {
			fmt.Printf("    %s → %s\n", result.Get("__name__"), result)
		}
	}
	fmt.Println()

	// 요약
	fmt.Println("  [파이프라인 요약]")
	fmt.Println("  ┌─────────────────────────────────────────────────────────┐")
	fmt.Println("  │  서비스 디스커버리                                         │")
	fmt.Println("  │  └→ __meta_kubernetes_* 레이블이 붙은 타겟 발견             │")
	fmt.Println("  ├─────────────────────────────────────────────────────────┤")
	fmt.Println("  │  relabel_configs (스크랩 전)                               │")
	fmt.Println("  │  ├→ keep/drop: 스크랩할 타겟 필터링                         │")
	fmt.Println("  │  ├→ replace: __meta_* → 사용자 레이블 변환                  │")
	fmt.Println("  │  ├→ labelmap: 패턴 기반 대량 레이블 매핑                     │")
	fmt.Println("  │  └→ labeldrop: __ 접두사 레이블 정리                        │")
	fmt.Println("  ├─────────────────────────────────────────────────────────┤")
	fmt.Println("  │  스크랩 → 메트릭 수집                                      │")
	fmt.Println("  ├─────────────────────────────────────────────────────────┤")
	fmt.Println("  │  metric_relabel_configs (스크랩 후)                         │")
	fmt.Println("  │  ├→ drop: 불필요한 메트릭 제거 (go_*, process_*)            │")
	fmt.Println("  │  └→ labeldrop: 고카디널리티 레이블 제거                      │")
	fmt.Println("  ├─────────────────────────────────────────────────────────┤")
	fmt.Println("  │  TSDB 저장                                               │")
	fmt.Println("  └─────────────────────────────────────────────────────────┘")
	fmt.Println()
}

// ---------------------------------------------------------------------------
// 헬퍼 함수
// ---------------------------------------------------------------------------

func printLabels(ls Labels, indent string) {
	for _, l := range ls {
		fmt.Printf("%s%-60s = %s\n", indent, l.Name, l.Value)
	}
}

func appendLabels(base, extra Labels) Labels {
	combined := make(Labels, 0, len(base)+len(extra))
	combined = append(combined, base...)
	combined = append(combined, extra...)
	sort.Sort(combined)
	return combined
}
