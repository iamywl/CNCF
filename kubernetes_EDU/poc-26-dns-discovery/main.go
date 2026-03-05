package main

import (
	"fmt"
	"strings"
)

// =============================================================================
// Kubernetes DNS & Service Discovery 시뮬레이션
// 참조:
//   - DNS Configurer: pkg/kubelet/network/dns/dns.go
//   - 환경변수 SD: pkg/kubelet/envvars/envvars.go
//   - Pod DNS: pkg/kubelet/kubelet_pods.go
//   - EndpointSlice: pkg/apis/discovery/types.go
// =============================================================================

// --- DNS Policy ---
// 실제: pkg/apis/core/types.go:3315-3335

type DNSPolicy string

const (
	DNSClusterFirstWithHostNet DNSPolicy = "ClusterFirstWithHostNet"
	DNSClusterFirst            DNSPolicy = "ClusterFirst" // 기본값
	DNSDefault                 DNSPolicy = "Default"
	DNSNone                    DNSPolicy = "None"
)

// --- DNS Config ---

type PodDNSConfig struct {
	Nameservers []string
	Searches    []string
	Options     []DNSOption
}

type DNSOption struct {
	Name  string
	Value string
}

// --- DNS Configurer ---
// 실제: pkg/kubelet/network/dns/dns.go:60-74

type DNSConfigurer struct {
	ClusterDNS    []string
	ClusterDomain string
	HostDNS       *PodDNSConfig // 노드의 resolv.conf
}

func NewDNSConfigurer(clusterDNS []string, clusterDomain string) *DNSConfigurer {
	return &DNSConfigurer{
		ClusterDNS:    clusterDNS,
		ClusterDomain: clusterDomain,
		HostDNS: &PodDNSConfig{
			Nameservers: []string{"8.8.8.8", "8.8.4.4"},
			Searches:    []string{"example.com"},
			Options:     []DNSOption{{Name: "ndots", Value: "1"}},
		},
	}
}

// GetPodDNS: Pod DNS 설정 생성
// 실제: dns.go:386-450
func (c *DNSConfigurer) GetPodDNS(namespace string, dnsPolicy DNSPolicy,
	hostNetwork bool, podDNSConfig *PodDNSConfig) *PodDNSConfig {

	var config PodDNSConfig

	switch dnsPolicy {
	case DNSNone:
		// 빈 config, podDNSConfig만 사용
		config = PodDNSConfig{}

	case DNSDefault:
		// 호스트 DNS 그대로 사용
		config = *c.HostDNS

	case DNSClusterFirst:
		if hostNetwork {
			// HostNetwork Pod: 호스트 DNS 사용
			config = *c.HostDNS
		} else {
			config = PodDNSConfig{
				Nameservers: c.ClusterDNS,
				Searches:    c.generateSearches(namespace),
				Options:     []DNSOption{{Name: "ndots", Value: "5"}},
			}
		}

	case DNSClusterFirstWithHostNet:
		config = PodDNSConfig{
			Nameservers: c.ClusterDNS,
			Searches:    c.generateSearches(namespace),
			Options:     []DNSOption{{Name: "ndots", Value: "5"}},
		}
	}

	// PodDNSConfig 병합 (사용자 커스텀)
	if podDNSConfig != nil {
		config = c.appendDNSConfig(config, podDNSConfig)
	}

	return &config
}

// generateSearchesForDNSClusterFirst
// 실제: dns.go:165-175
func (c *DNSConfigurer) generateSearches(namespace string) []string {
	nsSvcDomain := fmt.Sprintf("%s.svc.%s", namespace, c.ClusterDomain)
	svcDomain := fmt.Sprintf("svc.%s", c.ClusterDomain)

	searches := []string{nsSvcDomain, svcDomain, c.ClusterDomain}
	// 호스트 search 도 추가
	for _, s := range c.HostDNS.Searches {
		if !contains(searches, s) {
			searches = append(searches, s)
		}
	}
	return searches
}

func contains(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

// appendDNSConfig: PodDNSConfig 병합
// 실제: dns.go:378-383
func (c *DNSConfigurer) appendDNSConfig(base PodDNSConfig, custom *PodDNSConfig) PodDNSConfig {
	if len(custom.Nameservers) > 0 {
		base.Nameservers = append(base.Nameservers, custom.Nameservers...)
	}
	if len(custom.Searches) > 0 {
		base.Searches = append(base.Searches, custom.Searches...)
	}
	if len(custom.Options) > 0 {
		base.Options = mergeDNSOptions(base.Options, custom.Options)
	}
	return base
}

func mergeDNSOptions(base, custom []DNSOption) []DNSOption {
	optMap := make(map[string]DNSOption)
	for _, o := range base {
		optMap[o.Name] = o
	}
	for _, o := range custom {
		optMap[o.Name] = o // 덮어쓰기
	}
	var result []DNSOption
	for _, o := range optMap {
		result = append(result, o)
	}
	return result
}

// --- Service DNS 레코드 ---

type ServiceType string

const (
	ClusterIPSvc    ServiceType = "ClusterIP"
	HeadlessSvc     ServiceType = "Headless"
	ExternalNameSvc ServiceType = "ExternalName"
)

type ServiceDNS struct {
	Name         string
	Namespace    string
	Type         ServiceType
	ClusterIP    string
	ExternalName string
	Ports        []ServicePort
	PodIPs       []string // Headless Service용
}

type ServicePort struct {
	Name     string
	Port     int
	Protocol string
}

func (s *ServiceDNS) DNSRecords() []string {
	var records []string
	fqdn := fmt.Sprintf("%s.%s.svc.cluster.local", s.Name, s.Namespace)

	switch s.Type {
	case ClusterIPSvc:
		records = append(records, fmt.Sprintf("A    %s → %s", fqdn, s.ClusterIP))
		for _, p := range s.Ports {
			srv := fmt.Sprintf("_%s._%s.%s", p.Name, strings.ToLower(p.Protocol), fqdn)
			records = append(records, fmt.Sprintf("SRV  %s → %s:%d", srv, fqdn, p.Port))
		}

	case HeadlessSvc:
		for _, ip := range s.PodIPs {
			records = append(records, fmt.Sprintf("A    %s → %s", fqdn, ip))
		}
		for _, p := range s.Ports {
			for _, ip := range s.PodIPs {
				srv := fmt.Sprintf("_%s._%s.%s", p.Name, strings.ToLower(p.Protocol), fqdn)
				records = append(records, fmt.Sprintf("SRV  %s → %s:%d", srv, ip, p.Port))
			}
		}

	case ExternalNameSvc:
		records = append(records, fmt.Sprintf("CNAME %s → %s", fqdn, s.ExternalName))
	}

	return records
}

// --- Environment Variable 기반 Service Discovery ---
// 실제: pkg/kubelet/envvars/envvars.go:32-62

func FromServices(services []ServiceDNS) map[string]string {
	envs := make(map[string]string)

	for _, svc := range services {
		if svc.Type == HeadlessSvc || svc.Type == ExternalNameSvc {
			continue // ClusterIP가 없는 Service는 제외
		}

		// SERVICE_HOST, SERVICE_PORT
		prefix := makeEnvVariableName(svc.Name)
		envs[prefix+"_SERVICE_HOST"] = svc.ClusterIP
		if len(svc.Ports) > 0 {
			envs[prefix+"_SERVICE_PORT"] = fmt.Sprintf("%d", svc.Ports[0].Port)

			// 포트별 환경변수
			for _, p := range svc.Ports {
				if p.Name != "" {
					envs[prefix+"_SERVICE_PORT_"+strings.ToUpper(p.Name)] = fmt.Sprintf("%d", p.Port)
				}
				portKey := fmt.Sprintf("%s_PORT_%d_%s", prefix, p.Port, strings.ToUpper(p.Protocol))
				envs[portKey] = fmt.Sprintf("%s://%s:%d",
					strings.ToLower(p.Protocol), svc.ClusterIP, p.Port)
			}
		}
	}
	return envs
}

// makeEnvVariableName: 서비스명을 환경변수명으로 변환
// 실제: envvars.go:64-70
func makeEnvVariableName(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// --- Pod Hostname/Domain ---
// 실제: pkg/kubelet/kubelet_pods.go:580-617

func GeneratePodHostNameAndDomain(podName, hostname, subdomain, namespace, clusterDomain string) (string, string) {
	h := podName
	if hostname != "" {
		h = hostname
	}

	d := ""
	if subdomain != "" {
		d = fmt.Sprintf("%s.%s.svc.%s", subdomain, namespace, clusterDomain)
	}

	return h, d
}

// =============================================================================
// 데모
// =============================================================================

func main() {
	fmt.Println("=== Kubernetes DNS & Service Discovery 시뮬레이션 ===")
	fmt.Println()

	// 1. DNS Policy별 설정
	demo1_DNSPolicy()

	// 2. Search Domain 생성
	demo2_SearchDomain()

	// 3. Service DNS 레코드
	demo3_ServiceDNS()

	// 4. Environment Variable SD
	demo4_EnvVars()

	// 5. Pod Hostname/Domain
	demo5_PodHostname()

	// 6. DNS 해석 순서
	demo6_Resolution()

	printSummary()
}

func demo1_DNSPolicy() {
	fmt.Println("--- 1. DNS Policy별 설정 ---")

	c := NewDNSConfigurer([]string{"10.96.0.10"}, "cluster.local")

	policies := []struct {
		name        string
		policy      DNSPolicy
		hostNetwork bool
	}{
		{"ClusterFirst (기본)", DNSClusterFirst, false},
		{"ClusterFirst + HostNetwork", DNSClusterFirst, true},
		{"Default", DNSDefault, false},
		{"None + Custom", DNSNone, false},
	}

	for _, p := range policies {
		var custom *PodDNSConfig
		if p.policy == DNSNone {
			custom = &PodDNSConfig{
				Nameservers: []string{"1.1.1.1"},
				Searches:    []string{"custom.local"},
			}
		}

		config := c.GetPodDNS("production", p.policy, p.hostNetwork, custom)
		fmt.Printf("  %s:\n", p.name)
		fmt.Printf("    nameservers: %v\n", config.Nameservers)
		fmt.Printf("    search:      %v\n", config.Searches)
		if len(config.Options) > 0 {
			fmt.Printf("    options:     %s:%s\n", config.Options[0].Name, config.Options[0].Value)
		}
		fmt.Println()
	}
}

func demo2_SearchDomain() {
	fmt.Println("--- 2. Search Domain 생성 ---")

	c := NewDNSConfigurer([]string{"10.96.0.10"}, "cluster.local")
	searches := c.generateSearches("production")

	fmt.Println("  생성 순서 (namespace=production):")
	for i, s := range searches {
		fmt.Printf("    %d. %s\n", i+1, s)
	}
	fmt.Println()
	fmt.Println("  DNS 질의 시 'redis'를 찾으면:")
	fmt.Println("    1. redis.production.svc.cluster.local (✓ 네임스페이스 Service)")
	fmt.Println("    2. redis.svc.cluster.local")
	fmt.Println("    3. redis.cluster.local")
	fmt.Println("    4. redis.example.com (호스트 도메인)")
	fmt.Println()
}

func demo3_ServiceDNS() {
	fmt.Println("--- 3. Service DNS 레코드 ---")

	services := []ServiceDNS{
		{
			Name: "web", Namespace: "default", Type: ClusterIPSvc,
			ClusterIP: "10.96.1.100",
			Ports:     []ServicePort{{Name: "http", Port: 80, Protocol: "TCP"}},
		},
		{
			Name: "redis", Namespace: "default", Type: HeadlessSvc,
			PodIPs: []string{"10.244.1.10", "10.244.2.20", "10.244.3.30"},
			Ports:  []ServicePort{{Name: "redis", Port: 6379, Protocol: "TCP"}},
		},
		{
			Name: "ext-db", Namespace: "default", Type: ExternalNameSvc,
			ExternalName: "db.external.example.com",
		},
	}

	for _, svc := range services {
		fmt.Printf("  %s (Type=%s):\n", svc.Name, svc.Type)
		for _, record := range svc.DNSRecords() {
			fmt.Printf("    %s\n", record)
		}
		fmt.Println()
	}
}

func demo4_EnvVars() {
	fmt.Println("--- 4. Environment Variable 기반 SD ---")

	services := []ServiceDNS{
		{
			Name: "web-api", Namespace: "default", Type: ClusterIPSvc,
			ClusterIP: "10.96.1.100",
			Ports: []ServicePort{
				{Name: "http", Port: 80, Protocol: "TCP"},
				{Name: "grpc", Port: 9090, Protocol: "TCP"},
			},
		},
	}

	envs := FromServices(services)
	fmt.Println("  Service 'web-api' → 컨테이너 환경변수:")
	for k, v := range envs {
		fmt.Printf("    %s=%s\n", k, v)
	}
	fmt.Println()
}

func demo5_PodHostname() {
	fmt.Println("--- 5. Pod Hostname/Domain ---")

	cases := []struct {
		podName, hostname, subdomain, namespace string
	}{
		{"mysql-0", "", "", "default"},
		{"mysql-0", "db-master", "", "default"},
		{"mysql-0", "db-master", "mysql", "default"},
	}

	for _, c := range cases {
		h, d := GeneratePodHostNameAndDomain(c.podName, c.hostname, c.subdomain, c.namespace, "cluster.local")
		fqdn := h
		if d != "" {
			fqdn = h + "." + d
		}
		fmt.Printf("  pod=%s hostname=%q subdomain=%q → FQDN=%s\n",
			c.podName, c.hostname, c.subdomain, fqdn)
	}
	fmt.Println()
	fmt.Println("  Headless Service 'mysql' + subdomain='mysql' →")
	fmt.Println("    db-master.mysql.default.svc.cluster.local → Pod IP")
	fmt.Println()
}

func demo6_Resolution() {
	fmt.Println("--- 6. ndots:5의 의미 ---")
	fmt.Println()
	fmt.Println("  ndots:5 → 이름에 점(.)이 5개 미만이면 search domain을 먼저 시도")
	fmt.Println()
	fmt.Println("  'redis' (점 0개 < 5):")
	fmt.Println("    → redis.ns.svc.cluster.local 먼저 시도 (내부 Service)")
	fmt.Println()
	fmt.Println("  'api.example.com' (점 2개 < 5):")
	fmt.Println("    → api.example.com.ns.svc.cluster.local (불필요한 질의!)")
	fmt.Println("    → api.example.com.svc.cluster.local")
	fmt.Println("    → api.example.com.cluster.local")
	fmt.Println("    → api.example.com (최종 FQDN)")
	fmt.Println()
	fmt.Println("  'api.example.com.' (trailing dot):")
	fmt.Println("    → api.example.com 바로 질의 (절대 도메인)")
	fmt.Println()
}

func printSummary() {
	fmt.Println("=== 핵심 정리 ===")
	items := []string{
		"1. DNS Policy: ClusterFirst(기본) → 클러스터 DNS 사용, search domain 자동 추가",
		"2. Search Domain: {ns}.svc.{domain} → svc.{domain} → {domain} 순서",
		"3. ndots:5: 점 5개 미만이면 search domain 우선 (외부 도메인 trailing dot 권장)",
		"4. Service DNS: ClusterIP→A, Headless→Pod별 A, ExternalName→CNAME",
		"5. SRV 레코드: _{port-name}._{protocol}.{svc}.{ns}.svc.{domain}",
		"6. 환경변수 SD: {SVC}_SERVICE_HOST/PORT (ClusterIP가 있는 Service만)",
	}
	for _, item := range items {
		fmt.Printf("  %s\n", item)
	}
	fmt.Println()
	fmt.Println("소스코드 참조:")
	fmt.Println("  - DNS Configurer: pkg/kubelet/network/dns/dns.go")
	fmt.Println("  - 환경변수 SD:    pkg/kubelet/envvars/envvars.go")
	fmt.Println("  - Pod DNS:        pkg/kubelet/kubelet_pods.go")
}
