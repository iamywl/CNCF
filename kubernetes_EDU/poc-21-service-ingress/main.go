package main

import (
	"fmt"
	"math/rand"
	"net"
	"sort"
	"strings"
	"sync"
)

// =============================================================================
// Kubernetes Service & Ingress 시뮬레이션
// 참조:
//   - Service 할당: pkg/registry/core/service/storage/alloc.go
//   - Port Allocator: pkg/registry/core/service/portallocator/allocator.go
//   - IP Allocator: pkg/registry/core/service/ipallocator/ipallocator.go
//   - EndpointSlice: pkg/controller/endpointslice/endpointslice_controller.go
//   - Ingress: pkg/apis/networking/types.go
// =============================================================================

// --- ClusterIP Allocator ---

type IPAllocator struct {
	mu       sync.Mutex
	cidr     *net.IPNet
	used     map[string]bool
	size     int
	family   string // "IPv4" or "IPv6"
}

func NewIPAllocator(cidrStr, family string) *IPAllocator {
	_, cidr, _ := net.ParseCIDR(cidrStr)
	ones, bits := cidr.Mask.Size()
	size := 1 << (bits - ones) - 2 // 네트워크/브로드캐스트 제외

	return &IPAllocator{
		cidr:   cidr,
		used:   make(map[string]bool),
		size:   size,
		family: family,
	}
}

func (a *IPAllocator) Allocate(ip string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.used[ip] {
		return fmt.Errorf("IP %s already allocated", ip)
	}
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil || !a.cidr.Contains(parsedIP) {
		return fmt.Errorf("IP %s not in range %s", ip, a.cidr)
	}
	a.used[ip] = true
	return nil
}

func (a *IPAllocator) AllocateNext() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	ip := make(net.IP, len(a.cidr.IP))
	copy(ip, a.cidr.IP)

	// 간단한 순차 할당
	for i := 1; i <= a.size; i++ {
		candidate := incrementIP(ip, i)
		candidateStr := candidate.String()
		if !a.used[candidateStr] {
			a.used[candidateStr] = true
			return candidateStr, nil
		}
	}
	return "", fmt.Errorf("no available IPs in %s", a.cidr)
}

func (a *IPAllocator) Release(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.used, ip)
}

func incrementIP(baseIP net.IP, offset int) net.IP {
	ip := make(net.IP, len(baseIP))
	copy(ip, baseIP)
	for i := len(ip) - 1; i >= 0 && offset > 0; i-- {
		sum := int(ip[i]) + offset
		ip[i] = byte(sum % 256)
		offset = sum / 256
	}
	return ip
}

// --- NodePort Allocator ---

type PortAllocator struct {
	mu       sync.Mutex
	min, max int
	used     map[int]bool
}

func NewPortAllocator(min, max int) *PortAllocator {
	return &PortAllocator{
		min:  min,
		max:  max,
		used: make(map[int]bool),
	}
}

func (pa *PortAllocator) Allocate(port int) error {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	if port < pa.min || port > pa.max {
		return fmt.Errorf("port %d not in range [%d, %d]", port, pa.min, pa.max)
	}
	if pa.used[port] {
		return fmt.Errorf("port %d already allocated", port)
	}
	pa.used[port] = true
	return nil
}

func (pa *PortAllocator) AllocateNext() (int, error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	// 랜덤 오프셋으로 시작하여 충돌 최소화
	rangeSize := pa.max - pa.min + 1
	start := rand.Intn(rangeSize)

	for i := 0; i < rangeSize; i++ {
		port := pa.min + (start+i)%rangeSize
		if !pa.used[port] {
			pa.used[port] = true
			return port, nil
		}
	}
	return 0, fmt.Errorf("no available ports in range [%d, %d]", pa.min, pa.max)
}

// --- Service 타입 ---

type ServiceType string

const (
	ClusterIP    ServiceType = "ClusterIP"
	NodePort     ServiceType = "NodePort"
	LoadBalancer ServiceType = "LoadBalancer"
	ExternalName ServiceType = "ExternalName"
	Headless     ServiceType = "Headless" // ClusterIP="None"
)

type ServicePort struct {
	Name       string
	Port       int
	TargetPort int
	NodePort   int
	Protocol   string
}

type Service struct {
	Name                  string
	Namespace             string
	Type                  ServiceType
	ClusterIP             string
	ExternalName          string
	Ports                 []ServicePort
	Selector              map[string]string
	ExternalTrafficPolicy string // "Cluster" or "Local"
}

// --- EndpointSlice ---

type Endpoint struct {
	Address  string
	Ready    bool
	NodeName string
	Hostname string
}

type EndpointSlice struct {
	Name        string
	ServiceName string
	AddressType string
	Endpoints   []Endpoint
	Ports       []ServicePort
}

const maxEndpointsPerSlice = 100

// EndpointSlice Controller 시뮬레이션
type EndpointSliceController struct {
	slices map[string][]*EndpointSlice
}

func NewEndpointSliceController() *EndpointSliceController {
	return &EndpointSliceController{
		slices: make(map[string][]*EndpointSlice),
	}
}

func (c *EndpointSliceController) Reconcile(svc *Service, endpoints []Endpoint) {
	key := svc.Namespace + "/" + svc.Name

	// 엔드포인트를 maxEndpointsPerSlice로 분할
	var slices []*EndpointSlice
	for i := 0; i < len(endpoints); i += maxEndpointsPerSlice {
		end := i + maxEndpointsPerSlice
		if end > len(endpoints) {
			end = len(endpoints)
		}
		slice := &EndpointSlice{
			Name:        fmt.Sprintf("%s-%d", svc.Name, len(slices)),
			ServiceName: svc.Name,
			AddressType: "IPv4",
			Endpoints:   endpoints[i:end],
			Ports:       svc.Ports,
		}
		slices = append(slices, slice)
	}
	c.slices[key] = slices
	fmt.Printf("    EndpointSlice: %d개 생성 (%d endpoints)\n", len(slices), len(endpoints))
}

// --- Ingress ---

type PathType string

const (
	PathExact  PathType = "Exact"
	PathPrefix PathType = "Prefix"
)

type IngressPath struct {
	Path     string
	PathType PathType
	Backend  string // service:port
}

type IngressRule struct {
	Host  string
	Paths []IngressPath
}

type Ingress struct {
	Name           string
	IngressClass   string
	TLS            []string // hostnames
	Rules          []IngressRule
	DefaultBackend string
}

func (ing *Ingress) Route(host, path string) string {
	for _, rule := range ing.Rules {
		if rule.Host != host && rule.Host != "" {
			continue
		}
		for _, p := range rule.Paths {
			switch p.PathType {
			case PathExact:
				if path == p.Path {
					return p.Backend
				}
			case PathPrefix:
				if strings.HasPrefix(path, p.Path) {
					return p.Backend
				}
			}
		}
	}
	if ing.DefaultBackend != "" {
		return ing.DefaultBackend
	}
	return "(404 Not Found)"
}

// =============================================================================
// 데모
// =============================================================================

func main() {
	fmt.Println("=== Kubernetes Service & Ingress 시뮬레이션 ===")
	fmt.Println()

	// 1. ClusterIP 할당
	demo1_ClusterIP()

	// 2. NodePort 할당
	demo2_NodePort()

	// 3. Service 타입별 생성
	demo3_ServiceTypes()

	// 4. EndpointSlice 분할
	demo4_EndpointSlice()

	// 5. Ingress 라우팅
	demo5_Ingress()

	// 6. Traffic Policy
	demo6_TrafficPolicy()

	printSummary()
}

func demo1_ClusterIP() {
	fmt.Println("--- 1. ClusterIP 할당 ---")
	alloc := NewIPAllocator("10.96.0.0/16", "IPv4")

	// 순차 할당
	for i := 0; i < 3; i++ {
		ip, _ := alloc.AllocateNext()
		fmt.Printf("  AllocateNext() → %s\n", ip)
	}

	// 특정 IP 할당
	err := alloc.Allocate("10.96.100.1")
	fmt.Printf("  Allocate(10.96.100.1) → err=%v\n", err)

	// 중복 할당
	err = alloc.Allocate("10.96.100.1")
	fmt.Printf("  Allocate(10.96.100.1) 중복 → err=%v\n", err)
	fmt.Println()
}

func demo2_NodePort() {
	fmt.Println("--- 2. NodePort 할당 (30000-32767) ---")
	alloc := NewPortAllocator(30000, 32767)

	for i := 0; i < 3; i++ {
		port, _ := alloc.AllocateNext()
		fmt.Printf("  AllocateNext() → %d\n", port)
	}

	err := alloc.Allocate(31000)
	fmt.Printf("  Allocate(31000) → err=%v\n", err)
	fmt.Println()
}

func demo3_ServiceTypes() {
	fmt.Println("--- 3. Service 타입별 생성 ---")

	ipAlloc := NewIPAllocator("10.96.0.0/16", "IPv4")
	portAlloc := NewPortAllocator(30000, 32767)

	services := []Service{
		{Name: "web", Namespace: "default", Type: ClusterIP,
			Ports:    []ServicePort{{Name: "http", Port: 80, TargetPort: 8080}},
			Selector: map[string]string{"app": "web"}},
		{Name: "api", Namespace: "default", Type: NodePort,
			Ports:    []ServicePort{{Name: "http", Port: 80, TargetPort: 8080}},
			Selector: map[string]string{"app": "api"}},
		{Name: "frontend", Namespace: "default", Type: LoadBalancer,
			Ports:    []ServicePort{{Name: "https", Port: 443, TargetPort: 8443}},
			Selector: map[string]string{"app": "frontend"}},
		{Name: "external-db", Namespace: "default", Type: ExternalName,
			ExternalName: "db.example.com"},
		{Name: "stateful", Namespace: "default", Type: Headless,
			Ports:    []ServicePort{{Name: "grpc", Port: 9090, TargetPort: 9090}},
			Selector: map[string]string{"app": "stateful"}},
	}

	for i := range services {
		svc := &services[i]

		switch svc.Type {
		case ClusterIP:
			ip, _ := ipAlloc.AllocateNext()
			svc.ClusterIP = ip
			fmt.Printf("  %-15s Type=%-12s ClusterIP=%s\n", svc.Name, svc.Type, svc.ClusterIP)

		case NodePort:
			ip, _ := ipAlloc.AllocateNext()
			svc.ClusterIP = ip
			np, _ := portAlloc.AllocateNext()
			svc.Ports[0].NodePort = np
			fmt.Printf("  %-15s Type=%-12s ClusterIP=%s NodePort=%d\n",
				svc.Name, svc.Type, svc.ClusterIP, np)

		case LoadBalancer:
			ip, _ := ipAlloc.AllocateNext()
			svc.ClusterIP = ip
			np, _ := portAlloc.AllocateNext()
			svc.Ports[0].NodePort = np
			fmt.Printf("  %-15s Type=%-12s ClusterIP=%s NodePort=%d ExternalIP=203.0.113.10\n",
				svc.Name, svc.Type, svc.ClusterIP, np)

		case ExternalName:
			fmt.Printf("  %-15s Type=%-12s ExternalName=%s\n",
				svc.Name, svc.Type, svc.ExternalName)

		case Headless:
			svc.ClusterIP = "None"
			fmt.Printf("  %-15s Type=%-12s ClusterIP=%s (Headless)\n",
				svc.Name, svc.Type, svc.ClusterIP)
		}
	}
	fmt.Println()
}

func demo4_EndpointSlice() {
	fmt.Println("--- 4. EndpointSlice 분할 ---")

	ctrl := NewEndpointSliceController()
	svc := &Service{
		Name:      "large-service",
		Namespace: "default",
		Type:      ClusterIP,
		Ports:     []ServicePort{{Name: "http", Port: 80}},
	}

	// 250개 엔드포인트 → 3개 슬라이스
	var endpoints []Endpoint
	for i := 0; i < 250; i++ {
		endpoints = append(endpoints, Endpoint{
			Address:  fmt.Sprintf("10.244.%d.%d", i/256, i%256+1),
			Ready:    true,
			NodeName: fmt.Sprintf("worker-%d", i%3),
		})
	}

	ctrl.Reconcile(svc, endpoints)
	for _, slice := range ctrl.slices["default/large-service"] {
		fmt.Printf("    %s: %d endpoints\n", slice.Name, len(slice.Endpoints))
	}
	fmt.Println()
}

func demo5_Ingress() {
	fmt.Println("--- 5. Ingress 라우팅 ---")

	ing := &Ingress{
		Name:         "main-ingress",
		IngressClass: "nginx",
		TLS:          []string{"example.com"},
		DefaultBackend: "default-backend:80",
		Rules: []IngressRule{
			{
				Host: "example.com",
				Paths: []IngressPath{
					{Path: "/api", PathType: PathPrefix, Backend: "api-svc:80"},
					{Path: "/api/v2/health", PathType: PathExact, Backend: "health-svc:80"},
					{Path: "/", PathType: PathPrefix, Backend: "web-svc:80"},
				},
			},
		},
	}

	// 라우팅 순서: Exact → 긴 Prefix → 짧은 Prefix
	// Ingress Rules 내 Paths를 정렬
	for i := range ing.Rules {
		sort.Slice(ing.Rules[i].Paths, func(a, b int) bool {
			pa := ing.Rules[i].Paths[a]
			pb := ing.Rules[i].Paths[b]
			if pa.PathType == PathExact && pb.PathType != PathExact {
				return true
			}
			if pa.PathType != PathExact && pb.PathType == PathExact {
				return false
			}
			return len(pa.Path) > len(pb.Path)
		})
	}

	tests := []struct{ host, path string }{
		{"example.com", "/api/v2/health"},
		{"example.com", "/api/users"},
		{"example.com", "/dashboard"},
		{"other.com", "/anything"},
	}

	for _, t := range tests {
		backend := ing.Route(t.host, t.path)
		fmt.Printf("  %s%s → %s\n", t.host, t.path, backend)
	}
	fmt.Println()
}

func demo6_TrafficPolicy() {
	fmt.Println("--- 6. ExternalTrafficPolicy ---")

	fmt.Println("  Cluster (기본):")
	fmt.Println("    Client → NodePort(any) → kube-proxy → any Pod (SNAT)")
	fmt.Println("    ✓ 균등 분배  ✗ 클라이언트 IP 손실")
	fmt.Println()
	fmt.Println("  Local:")
	fmt.Println("    Client → NodePort(node) → local Pod only (no SNAT)")
	fmt.Println("    ✓ 클라이언트 IP 보존  ✗ 불균등 분배 가능")
	fmt.Println()
}

func printSummary() {
	fmt.Println("=== 핵심 정리 ===")
	items := []string{
		"1. ClusterIP: IPAllocator로 ServiceCIDR 범위에서 할당 (듀얼 스택 지원)",
		"2. NodePort: PortAllocator로 30000-32767 범위 할당",
		"3. EndpointSlice: 최대 100 endpoints/slice, Service당 여러 슬라이스",
		"4. Ingress: PathType(Exact/Prefix) + Host 기반 라우팅",
		"5. ExternalTrafficPolicy: Cluster(균등) vs Local(IP 보존)",
		"6. Headless Service: ClusterIP=None → Pod IP 직접 반환",
	}
	for _, item := range items {
		fmt.Printf("  %s\n", item)
	}
}
