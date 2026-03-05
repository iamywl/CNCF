package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// =============================================================================
// Kubernetes kubectl 내부 구조 시뮬레이션
// Cobra 명령 구조, Factory 패턴, Apply, Resource Printer
// 참조:
//   - cmd.go: staging/src/k8s.io/kubectl/pkg/cmd/cmd.go
//   - apply: staging/src/k8s.io/kubectl/pkg/cmd/apply/apply.go
//   - get: staging/src/k8s.io/kubectl/pkg/cmd/get/get.go
//   - plugin: staging/src/k8s.io/kubectl/pkg/cmd/plugin.go
// =============================================================================

// --- Cobra Command 시뮬레이션 ---
// 실제: cobra.Command 기반 CLI 구조

type Command struct {
	Use     string
	Short   string
	RunFunc func(args []string)
	Sub     []*Command
}

func (c *Command) AddCommand(sub *Command) {
	c.Sub = append(c.Sub, sub)
}

func (c *Command) Execute(args []string) {
	if len(args) > 0 {
		for _, sub := range c.Sub {
			if sub.Use == args[0] {
				sub.Execute(args[1:])
				return
			}
		}
	}
	if c.RunFunc != nil {
		c.RunFunc(args)
	}
}

// --- Factory 패턴 ---
// 실제: staging/src/k8s.io/kubectl/pkg/cmd/util/factory.go:41

type Factory interface {
	ToRESTConfig() *RESTConfig
	NewBuilder() *ResourceBuilder
	DynamicClient() *DynamicClient
}

type RESTConfig struct {
	Host        string
	BearerToken string
	TLSVerify   bool
}

type DynamicClient struct {
	config *RESTConfig
}

type factoryImpl struct {
	kubeconfig string
	context    string
	config     *RESTConfig
}

func NewFactory(kubeconfig, context string) Factory {
	return &factoryImpl{
		kubeconfig: kubeconfig,
		context:    context,
		config: &RESTConfig{
			Host:        "https://api.example.com:6443",
			BearerToken: "token-xxx",
			TLSVerify:   true,
		},
	}
}

func (f *factoryImpl) ToRESTConfig() *RESTConfig   { return f.config }
func (f *factoryImpl) NewBuilder() *ResourceBuilder { return &ResourceBuilder{config: f.config} }
func (f *factoryImpl) DynamicClient() *DynamicClient {
	return &DynamicClient{config: f.config}
}

// --- Resource Builder ---

type Resource struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	Spec       map[string]interface{} `json:"spec,omitempty"`
}

type ResourceBuilder struct {
	config    *RESTConfig
	resources []Resource
}

func (b *ResourceBuilder) FromFile(resources []Resource) *ResourceBuilder {
	b.resources = resources
	return b
}

func (b *ResourceBuilder) Do() []Resource {
	return b.resources
}

// --- Apply 시뮬레이션 ---
// 실제: staging/src/k8s.io/kubectl/pkg/cmd/apply/apply.go:82

type ApplyOptions struct {
	ServerSideApply bool
	FieldManager    string
	ForceConflicts  bool
	DryRun          bool
	existingObjects map[string]Resource // 서버 상태 시뮬레이션
}

func NewApplyOptions() *ApplyOptions {
	return &ApplyOptions{
		FieldManager:    "kubectl-client-side-apply",
		existingObjects: make(map[string]Resource),
	}
}

func (o *ApplyOptions) Apply(resource Resource) string {
	key := resource.Namespace + "/" + resource.Kind + "/" + resource.Name

	if o.DryRun {
		return fmt.Sprintf("  [apply] %s (dry-run)", key)
	}

	_, exists := o.existingObjects[key]
	o.existingObjects[key] = resource

	method := "created"
	if exists {
		method = "configured"
	}

	if o.ServerSideApply {
		return fmt.Sprintf("  [apply] %s serverside-applied (manager=%s)", key, o.FieldManager)
	}
	return fmt.Sprintf("  [apply] %s/%s %s", resource.Kind, resource.Name, method)
}

// --- Resource Printer ---
// 실제: staging/src/k8s.io/kubectl/pkg/cmd/get/get.go

type OutputFormat string

const (
	FormatTable        OutputFormat = "table"
	FormatJSON         OutputFormat = "json"
	FormatYAML         OutputFormat = "yaml"
	FormatCustomColumn OutputFormat = "custom-columns"
)

type ResourcePrinter interface {
	Print(resources []Resource)
}

// Table Printer
type TablePrinter struct{}

func (p *TablePrinter) Print(resources []Resource) {
	fmt.Printf("  %-15s %-15s %-10s\n", "NAME", "KIND", "NAMESPACE")
	for _, r := range resources {
		fmt.Printf("  %-15s %-15s %-10s\n", r.Name, r.Kind, r.Namespace)
	}
}

// JSON Printer
type JSONPrinter struct{}

func (p *JSONPrinter) Print(resources []Resource) {
	data, _ := json.MarshalIndent(resources, "  ", "  ")
	fmt.Printf("  %s\n", string(data))
}

// Custom Columns Printer
// 실제: staging/src/k8s.io/kubectl/pkg/cmd/get/customcolumn.go:75
type CustomColumnsPrinter struct {
	Columns []ColumnDef
}

type ColumnDef struct {
	Header   string
	JSONPath string
}

func ParseCustomColumns(spec string) *CustomColumnsPrinter {
	var cols []ColumnDef
	parts := strings.Split(spec, ",")
	for _, part := range parts {
		kv := strings.SplitN(part, ":", 2)
		if len(kv) == 2 {
			cols = append(cols, ColumnDef{Header: kv[0], JSONPath: kv[1]})
		}
	}
	return &CustomColumnsPrinter{Columns: cols}
}

func (p *CustomColumnsPrinter) Print(resources []Resource) {
	// 헤더
	for _, col := range p.Columns {
		fmt.Printf("  %-20s", col.Header)
	}
	fmt.Println()

	// 데이터 (간단한 필드 접근)
	for _, r := range resources {
		for _, col := range p.Columns {
			val := resolveJSONPath(r, col.JSONPath)
			fmt.Printf("  %-20s", val)
		}
		fmt.Println()
	}
}

func resolveJSONPath(r Resource, path string) string {
	switch path {
	case ".name":
		return r.Name
	case ".kind":
		return r.Kind
	case ".namespace":
		return r.Namespace
	case ".apiVersion":
		return r.APIVersion
	default:
		return "<unknown>"
	}
}

// --- Plugin 메커니즘 ---
// 실제: staging/src/k8s.io/kubectl/pkg/cmd/plugin.go:32

type PluginHandler struct {
	ValidPrefixes []string
}

func NewPluginHandler() *PluginHandler {
	return &PluginHandler{ValidPrefixes: []string{"kubectl"}}
}

func (h *PluginHandler) Lookup(name string) (string, bool) {
	for _, prefix := range h.ValidPrefixes {
		pluginName := fmt.Sprintf("%s-%s", prefix, name)
		// 실제로는 exec.LookPath() 사용
		// 시뮬레이션: PATH에서 검색하는 대신 존재 여부만 확인
		if _, err := os.Stat("/usr/local/bin/" + pluginName); err == nil {
			return "/usr/local/bin/" + pluginName, true
		}
	}
	return "", false
}

// --- kubeconfig 로딩 ---

type KubeConfig struct {
	Clusters []ClusterEntry
	Contexts []ContextEntry
	Users    []UserEntry
	Current  string
}

type ClusterEntry struct {
	Name   string
	Server string
}

type ContextEntry struct {
	Name      string
	Cluster   string
	User      string
	Namespace string
}

type UserEntry struct {
	Name  string
	Token string
}

func LoadKubeConfig() *KubeConfig {
	return &KubeConfig{
		Clusters: []ClusterEntry{
			{Name: "production", Server: "https://k8s-prod.example.com:6443"},
			{Name: "staging", Server: "https://k8s-staging.example.com:6443"},
		},
		Contexts: []ContextEntry{
			{Name: "prod-admin", Cluster: "production", User: "admin", Namespace: "default"},
			{Name: "staging-dev", Cluster: "staging", User: "developer", Namespace: "dev"},
		},
		Users: []UserEntry{
			{Name: "admin", Token: "admin-token-xxx"},
			{Name: "developer", Token: "dev-token-yyy"},
		},
		Current: "prod-admin",
	}
}

// =============================================================================
// 데모
// =============================================================================

func main() {
	fmt.Println("=== kubectl 내부 구조 시뮬레이션 ===")
	fmt.Println()

	// 1. Cobra 명령 구조
	demo1_CobraStructure()

	// 2. kubeconfig 로딩
	demo2_KubeConfig()

	// 3. Factory 패턴
	demo3_Factory()

	// 4. kubectl apply
	demo4_Apply()

	// 5. Resource Printer
	demo5_Printer()

	// 6. Plugin 메커니즘
	demo6_Plugin()

	printSummary()
}

func demo1_CobraStructure() {
	fmt.Println("--- 1. Cobra 명령 구조 ---")

	root := &Command{Use: "kubectl", Short: "Kubernetes CLI"}

	// Basic Commands
	get := &Command{Use: "get", Short: "Display resources", RunFunc: func(args []string) {
		fmt.Printf("  kubectl get %s\n", strings.Join(args, " "))
	}}
	apply := &Command{Use: "apply", Short: "Apply configuration", RunFunc: func(args []string) {
		fmt.Printf("  kubectl apply %s\n", strings.Join(args, " "))
	}}
	root.AddCommand(get)
	root.AddCommand(apply)

	// 실행
	fmt.Println("  명령 트리:")
	fmt.Println("  kubectl")
	for _, sub := range root.Sub {
		fmt.Printf("    ├── %s: %s\n", sub.Use, sub.Short)
	}

	fmt.Println()
	root.Execute([]string{"get", "pods"})
	root.Execute([]string{"apply", "-f", "deployment.yaml"})
	fmt.Println()
}

func demo2_KubeConfig() {
	fmt.Println("--- 2. kubeconfig 로딩 ---")
	fmt.Println("  우선순위:")
	fmt.Println("    1. --kubeconfig 플래그")
	fmt.Println("    2. $KUBECONFIG 환경변수")
	fmt.Println("    3. ~/.kube/config")
	fmt.Println()

	config := LoadKubeConfig()
	fmt.Printf("  현재 컨텍스트: %s\n", config.Current)
	for _, ctx := range config.Contexts {
		marker := "  "
		if ctx.Name == config.Current {
			marker = "* "
		}
		fmt.Printf("  %s%-15s cluster=%-12s user=%-12s ns=%s\n",
			marker, ctx.Name, ctx.Cluster, ctx.User, ctx.Namespace)
	}
	fmt.Println()
}

func demo3_Factory() {
	fmt.Println("--- 3. Factory 패턴 ---")

	f := NewFactory("~/.kube/config", "prod-admin")
	config := f.ToRESTConfig()
	fmt.Printf("  REST Config: host=%s, tls=%v\n", config.Host, config.TLSVerify)

	builder := f.NewBuilder()
	resources := builder.FromFile([]Resource{
		{Kind: "Deployment", Name: "web", Namespace: "default"},
	}).Do()
	fmt.Printf("  Builder: %d resources loaded\n", len(resources))
	fmt.Println()
}

func demo4_Apply() {
	fmt.Println("--- 4. kubectl apply ---")

	// Client-Side Apply
	fmt.Println("  Client-Side Apply:")
	opts := NewApplyOptions()
	result := opts.Apply(Resource{Kind: "Deployment", Name: "web", Namespace: "default"})
	fmt.Println(result)
	result = opts.Apply(Resource{Kind: "Deployment", Name: "web", Namespace: "default"})
	fmt.Println(result)

	// Server-Side Apply
	fmt.Println("\n  Server-Side Apply:")
	ssaOpts := NewApplyOptions()
	ssaOpts.ServerSideApply = true
	ssaOpts.FieldManager = "my-controller"
	result = ssaOpts.Apply(Resource{Kind: "Deployment", Name: "web", Namespace: "default"})
	fmt.Println(result)

	// Dry Run
	fmt.Println("\n  Dry Run:")
	dryOpts := NewApplyOptions()
	dryOpts.DryRun = true
	result = dryOpts.Apply(Resource{Kind: "Deployment", Name: "web", Namespace: "default"})
	fmt.Println(result)
	fmt.Println()
}

func demo5_Printer() {
	fmt.Println("--- 5. Resource Printer ---")

	resources := []Resource{
		{APIVersion: "apps/v1", Kind: "Deployment", Name: "web", Namespace: "default"},
		{APIVersion: "v1", Kind: "Service", Name: "web-svc", Namespace: "default"},
		{APIVersion: "v1", Kind: "ConfigMap", Name: "config", Namespace: "kube-system"},
	}

	// Table 형식
	fmt.Println("  Format=table:")
	tp := &TablePrinter{}
	tp.Print(resources)
	fmt.Println()

	// JSON 형식
	fmt.Println("  Format=json (첫 번째만):")
	jp := &JSONPrinter{}
	jp.Print(resources[:1])
	fmt.Println()

	// Custom Columns
	fmt.Println("  Format=custom-columns:")
	cp := ParseCustomColumns("NAME:.name,KIND:.kind,APIVERSION:.apiVersion")
	cp.Print(resources)
	fmt.Println()
}

func demo6_Plugin() {
	fmt.Println("--- 6. Plugin 메커니즘 ---")
	fmt.Println("  검색 패턴: kubectl-{name} in $PATH")
	fmt.Println("  예시:")
	fmt.Println("    kubectl myplugin → kubectl-myplugin")
	fmt.Println("    kubectl create myplugin → kubectl-create-myplugin")
	fmt.Println()

	ph := NewPluginHandler()
	// 실제로는 PATH를 검색하지만, 시뮬레이션에서는 존재하지 않음
	path, found := ph.Lookup("debug")
	fmt.Printf("  Lookup('debug'): found=%v, path=%q\n", found, path)

	// 프로세스 교체 방식
	fmt.Println("  실행 방식: Unix=syscall.Exec (프로세스 교체)")
	fmt.Println()
}

func printSummary() {
	fmt.Println("=== 핵심 정리 ===")
	items := []string{
		"1. kubectl은 Cobra 기반 CLI로 명령 그룹(Basic/Deploy/Debug/Advanced) 구조",
		"2. Factory 패턴으로 kubeconfig → REST Config → Client 생성 추상화",
		"3. Apply: CSA(3-way merge + last-applied 주석) vs SSA(FieldManager 소유권)",
		"4. Get: Resource Builder → Table/JSON/YAML/CustomColumns Printer",
		"5. Exec/Logs: SPDY/WebSocket 프로토콜로 컨테이너 접속",
		"6. Plugin: PATH의 kubectl-* 바이너리를 자동 발견하여 실행",
	}
	for _, item := range items {
		fmt.Printf("  %s\n", item)
	}
	fmt.Println()
	fmt.Println("소스코드 참조:")
	fmt.Println("  - cmd.go:    staging/src/k8s.io/kubectl/pkg/cmd/cmd.go")
	fmt.Println("  - apply.go:  staging/src/k8s.io/kubectl/pkg/cmd/apply/apply.go")
	fmt.Println("  - get.go:    staging/src/k8s.io/kubectl/pkg/cmd/get/get.go")
	fmt.Println("  - plugin.go: staging/src/k8s.io/kubectl/pkg/cmd/plugin.go")
}
