// poc-07-service-discovery: CoreDNS Kubernetes 플러그인의 서비스 디스커버리 시뮬레이션
//
// CoreDNS kubernetes 플러그인(plugin/kubernetes/)의 서비스 → DNS 매핑을
// Kubernetes DNS 스펙(https://github.com/kubernetes/dns/blob/master/docs/specification.md)에
// 따라 재현한다.
//
// 실제 소스 참조:
//   - plugin/kubernetes/kubernetes.go: Records(), findServices(), findPods()
//   - plugin/kubernetes/handler.go: ServeDNS() - 쿼리 타입별 처리
//   - plugin/kubernetes/parse.go: parseRequest() - DNS 이름 파싱
//   - plugin/kubernetes/reverse.go: Reverse(), serviceRecordForIP()
//
// 실행: go run main.go

package main

import (
	"fmt"
	"net"
	"strings"
	"sync"
)

// ============================================================================
// 1. Kubernetes 데이터 모델
// ============================================================================

// ServiceType은 Kubernetes 서비스 타입
type ServiceType int

const (
	ClusterIPService  ServiceType = iota // 일반 ClusterIP 서비스
	HeadlessService                      // Headless (ClusterIP=None)
	ExternalName                         // ExternalName
)

// Port는 서비스 포트 정의
type Port struct {
	Name     string // 포트 이름 (e.g., "http", "grpc")
	Port     int    // 포트 번호
	Protocol string // TCP, UDP
}

// EndpointAddress는 엔드포인트 주소
type EndpointAddress struct {
	IP       string
	Hostname string
}

// Endpoint는 서비스의 엔드포인트 (Pod IP + Port)
type Endpoint struct {
	Addresses []EndpointAddress
	Ports     []Port
}

// Service는 Kubernetes 서비스를 나타낸다
type Service struct {
	Name         string
	Namespace    string
	Type         ServiceType
	ClusterIPs   []string    // ClusterIP 주소
	Ports        []Port      // 서비스 포트
	ExternalName string      // ExternalName 타입일 때
	Endpoints    []Endpoint  // 엔드포인트 (Headless 서비스용)
	TTL          uint32
}

// ============================================================================
// 2. DNS 레코드 타입
// ============================================================================

const (
	TypeA   uint16 = 1
	TypeAAAA uint16 = 28
	TypeSRV uint16 = 33
	TypePTR uint16 = 12
	TypeSOA uint16 = 6
	TypeNS  uint16 = 2
	TypeCNAME uint16 = 5
)

// DNSRecord는 DNS 응답 레코드
type DNSRecord struct {
	Name     string
	Type     uint16
	Value    string
	TTL      uint32
	Port     int    // SRV 전용
	Priority int    // SRV 전용
	Weight   int    // SRV 전용
}

// TypeString은 레코드 타입 문자열을 반환
func (r DNSRecord) TypeString() string {
	switch r.Type {
	case TypeA:
		return "A"
	case TypeAAAA:
		return "AAAA"
	case TypeSRV:
		return "SRV"
	case TypePTR:
		return "PTR"
	case TypeSOA:
		return "SOA"
	case TypeCNAME:
		return "CNAME"
	default:
		return fmt.Sprintf("TYPE%d", r.Type)
	}
}

// ============================================================================
// 3. DNS 이름 파서 - plugin/kubernetes/parse.go의 parseRequest() 재현
// ============================================================================

// RecordRequest는 파싱된 DNS 쿼리 요청
// 실제 소스: plugin/kubernetes/parse.go의 recordRequest 구조체
type RecordRequest struct {
	Port      string // SRV의 _port 부분
	Protocol  string // SRV의 _protocol 부분
	Endpoint  string // 엔드포인트 이름
	Service   string // 서비스 이름
	Namespace string // 네임스페이스
	PodOrSvc  string // "pod" 또는 "svc"
}

// parseRequest는 DNS 이름을 파싱하여 Kubernetes 쿼리 요소를 추출한다
// 실제 소스: plugin/kubernetes/parse.go의 func parseRequest()
//
// 가능한 형식:
//   1. _port._protocol.service.namespace.svc.zone
//   2. endpoint.service.namespace.svc.zone
//   3. service.namespace.svc.zone
func parseRequest(name, zone string) (RecordRequest, error) {
	r := RecordRequest{}

	// 존 접미사 제거
	base := trimZone(name, zone)
	if base == "" || base == "svc" || base == "pod" {
		return r, nil
	}

	segs := splitDomainName(base)
	last := len(segs) - 1
	if last < 0 {
		return r, nil
	}

	// 마지막 세그먼트: "svc" 또는 "pod"
	r.PodOrSvc = segs[last]
	if r.PodOrSvc != "svc" && r.PodOrSvc != "pod" {
		return r, fmt.Errorf("잘못된 요청: pod/svc 필요, got %q", r.PodOrSvc)
	}
	last--
	if last < 0 {
		return r, nil
	}

	// 네임스페이스
	r.Namespace = segs[last]
	last--
	if last < 0 {
		return r, nil
	}

	// 서비스 이름
	r.Service = segs[last]
	last--
	if last < 0 {
		return r, nil
	}

	// 나머지 세그먼트: 포트/프로토콜 또는 엔드포인트
	switch last {
	case 0:
		// 엔드포인트만
		r.Endpoint = segs[0]
	case 1:
		// _port._protocol
		r.Protocol = stripUnderscore(segs[1])
		r.Port = stripUnderscore(segs[0])
	}

	return r, nil
}

func trimZone(name, zone string) string {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	zone = strings.ToLower(strings.TrimSuffix(zone, "."))
	if strings.HasSuffix(name, zone) {
		name = strings.TrimSuffix(name, zone)
		name = strings.TrimSuffix(name, ".")
	}
	return name
}

func splitDomainName(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ".")
}

func stripUnderscore(s string) string {
	return strings.TrimPrefix(s, "_")
}

// ============================================================================
// 4. 서비스 레지스트리 - plugin/kubernetes/kubernetes.go의 인메모리 캐시 재현
// ============================================================================

// ServiceRegistry는 서비스와 엔드포인트의 인메모리 저장소
type ServiceRegistry struct {
	services map[string]*Service // key: "name.namespace"
	mu       sync.RWMutex
}

// NewServiceRegistry는 새 레지스트리를 생성한다
func NewServiceRegistry() *ServiceRegistry {
	return &ServiceRegistry{
		services: make(map[string]*Service),
	}
}

// Register는 서비스를 등록한다
func (r *ServiceRegistry) Register(svc *Service) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := svc.Name + "." + svc.Namespace
	r.services[key] = svc
}

// Lookup은 서비스를 조회한다
// 실제 소스: plugin/kubernetes/kubernetes.go의 findServices()
func (r *ServiceRegistry) Lookup(name, namespace string) *Service {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key := strings.ToLower(name) + "." + strings.ToLower(namespace)
	return r.services[key]
}

// LookupByIP는 IP로 서비스를 역방향 조회한다
// 실제 소스: plugin/kubernetes/reverse.go의 serviceRecordForIP()
func (r *ServiceRegistry) LookupByIP(ip string) *Service {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, svc := range r.services {
		for _, cip := range svc.ClusterIPs {
			if cip == ip {
				return svc
			}
		}
	}
	return nil
}

// AllServices는 모든 서비스를 반환한다
func (r *ServiceRegistry) AllServices() []*Service {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*Service
	for _, svc := range r.services {
		result = append(result, svc)
	}
	return result
}

// ============================================================================
// 5. DNS 서버 - plugin/kubernetes/handler.go의 ServeDNS() 재현
// ============================================================================

// KubeDNSServer는 Kubernetes DNS를 시뮬레이션한다
type KubeDNSServer struct {
	Zone     string // e.g., "cluster.local."
	Registry *ServiceRegistry
	DefaultTTL uint32
}

// NewKubeDNSServer는 새 DNS 서버를 생성한다
func NewKubeDNSServer(zone string) *KubeDNSServer {
	return &KubeDNSServer{
		Zone:       zone,
		Registry:   NewServiceRegistry(),
		DefaultTTL: 5, // CoreDNS kubernetes 플러그인 기본 TTL
	}
}

// Query는 DNS 쿼리를 처리한다
// 실제 소스: plugin/kubernetes/handler.go의 func (k Kubernetes) ServeDNS()
func (s *KubeDNSServer) Query(qname string, qtype uint16) ([]DNSRecord, int) {
	qname = strings.ToLower(qname)

	// 역방향 조회 확인
	if strings.HasSuffix(qname, ".in-addr.arpa.") {
		return s.handleReverse(qname)
	}

	// 쿼리 이름 파싱
	req, err := parseRequest(qname, s.Zone)
	if err != nil {
		return nil, 3 // NXDOMAIN
	}

	if req.PodOrSvc == "" {
		return nil, 3
	}

	// 서비스 조회
	svc := s.Registry.Lookup(req.Service, req.Namespace)
	if svc == nil {
		return nil, 3 // NXDOMAIN
	}

	// 쿼리 타입에 따라 처리
	// 실제 소스: handler.go의 switch state.QType()
	switch qtype {
	case TypeA:
		return s.handleA(svc, req)
	case TypeSRV:
		return s.handleSRV(svc, req)
	case TypePTR:
		return s.handleReverse(qname)
	default:
		return nil, 0 // NODATA
	}
}

// handleA는 A 레코드 쿼리를 처리한다
// 실제 소스: plugin/kubernetes/kubernetes.go의 findServices() - ClusterIP 서비스
func (s *KubeDNSServer) handleA(svc *Service, req RecordRequest) ([]DNSRecord, int) {
	switch svc.Type {
	case ClusterIPService:
		// ClusterIP → A 레코드 반환
		var records []DNSRecord
		for _, ip := range svc.ClusterIPs {
			name := fmt.Sprintf("%s.%s.svc.%s", svc.Name, svc.Namespace, s.Zone)
			records = append(records, DNSRecord{
				Name:  name,
				Type:  TypeA,
				Value: ip,
				TTL:   svc.TTL,
			})
		}
		if len(records) == 0 {
			return nil, 3
		}
		return records, 0

	case HeadlessService:
		// Headless → 각 엔드포인트의 IP 반환
		var records []DNSRecord
		for _, ep := range svc.Endpoints {
			for _, addr := range ep.Addresses {
				name := fmt.Sprintf("%s.%s.svc.%s", svc.Name, svc.Namespace, s.Zone)
				records = append(records, DNSRecord{
					Name:  name,
					Type:  TypeA,
					Value: addr.IP,
					TTL:   svc.TTL,
				})
			}
		}
		if len(records) == 0 {
			return nil, 3
		}
		return records, 0

	case ExternalName:
		// ExternalName → CNAME 반환
		name := fmt.Sprintf("%s.%s.svc.%s", svc.Name, svc.Namespace, s.Zone)
		return []DNSRecord{{
			Name:  name,
			Type:  TypeCNAME,
			Value: svc.ExternalName,
			TTL:   svc.TTL,
		}}, 0

	default:
		return nil, 3
	}
}

// handleSRV는 SRV 레코드 쿼리를 처리한다
// 실제 소스: handler.go에서 dns.TypeSRV 분기
// SRV 형식: _port._protocol.service.namespace.svc.zone
func (s *KubeDNSServer) handleSRV(svc *Service, req RecordRequest) ([]DNSRecord, int) {
	var records []DNSRecord

	switch svc.Type {
	case ClusterIPService:
		for _, p := range svc.Ports {
			if req.Port != "" && !strings.EqualFold(req.Port, p.Name) {
				continue
			}
			if req.Protocol != "" && !strings.EqualFold(req.Protocol, p.Protocol) {
				continue
			}
			target := fmt.Sprintf("%s.%s.svc.%s", svc.Name, svc.Namespace, s.Zone)
			records = append(records, DNSRecord{
				Name:     fmt.Sprintf("_%s._%s.%s", p.Name, strings.ToLower(p.Protocol), target),
				Type:     TypeSRV,
				Value:    target,
				TTL:      svc.TTL,
				Port:     p.Port,
				Priority: 0,
				Weight:   100,
			})
		}

	case HeadlessService:
		// Headless 서비스: 각 엔드포인트에 대해 SRV 레코드
		for _, ep := range svc.Endpoints {
			for _, addr := range ep.Addresses {
				for _, p := range ep.Ports {
					if req.Port != "" && !strings.EqualFold(req.Port, p.Name) {
						continue
					}
					hostname := endpointHostname(addr)
					target := fmt.Sprintf("%s.%s.%s.svc.%s",
						hostname, svc.Name, svc.Namespace, s.Zone)
					records = append(records, DNSRecord{
						Name:     target,
						Type:     TypeSRV,
						Value:    target,
						TTL:      svc.TTL,
						Port:     p.Port,
						Priority: 0,
						Weight:   100,
					})
				}
			}
		}
	}

	if len(records) == 0 {
		return nil, 3
	}
	return records, 0
}

// handleReverse는 PTR 역방향 쿼리를 처리한다
// 실제 소스: plugin/kubernetes/reverse.go의 Reverse()와 serviceRecordForIP()
func (s *KubeDNSServer) handleReverse(qname string) ([]DNSRecord, int) {
	ip := extractIPFromReverse(qname)
	if ip == "" {
		return nil, 3
	}

	svc := s.Registry.LookupByIP(ip)
	if svc == nil {
		return nil, 3
	}

	// PTR → 서비스 FQDN 반환
	domain := fmt.Sprintf("%s.%s.svc.%s", svc.Name, svc.Namespace, s.Zone)
	return []DNSRecord{{
		Name:  qname,
		Type:  TypePTR,
		Value: domain,
		TTL:   svc.TTL,
	}}, 0
}

// endpointHostname은 엔드포인트의 호스트명을 생성한다
// 실제 소스: plugin/kubernetes/kubernetes.go의 endpointHostname()
func endpointHostname(addr EndpointAddress) string {
	if addr.Hostname != "" {
		return addr.Hostname
	}
	// IP의 .을 -로 변환
	if strings.Contains(addr.IP, ".") {
		return strings.ReplaceAll(addr.IP, ".", "-")
	}
	// IPv6
	if strings.Contains(addr.IP, ":") {
		h := strings.ReplaceAll(addr.IP, ":", "-")
		if strings.HasSuffix(h, "-") {
			return h + "0"
		}
		return h
	}
	return ""
}

// extractIPFromReverse는 역방향 DNS 이름에서 IP를 추출한다
func extractIPFromReverse(name string) string {
	name = strings.TrimSuffix(name, ".")
	if !strings.HasSuffix(name, ".in-addr.arpa") {
		return ""
	}
	name = strings.TrimSuffix(name, ".in-addr.arpa")
	parts := strings.Split(name, ".")
	// 옥텟 순서 반전
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	ip := strings.Join(parts, ".")
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
}

// ipToReverse는 IP를 역방향 DNS 이름으로 변환한다
func ipToReverse(ip string) string {
	parts := strings.Split(ip, ".")
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, ".") + ".in-addr.arpa."
}

// ============================================================================
// 6. 데모 실행
// ============================================================================

func main() {
	fmt.Println("=== CoreDNS 서비스 디스커버리 PoC ===")
	fmt.Println()
	fmt.Println("CoreDNS kubernetes 플러그인의 서비스 → DNS 매핑을 시뮬레이션합니다.")
	fmt.Println("Kubernetes DNS 스펙 v1.1.0을 따릅니다.")
	fmt.Println("참조: plugin/kubernetes/kubernetes.go, handler.go, parse.go, reverse.go")
	fmt.Println()

	server := NewKubeDNSServer("cluster.local.")

	// ── 서비스 등록 ──
	fmt.Println("── 서비스 등록 ──")
	fmt.Println()

	services := []*Service{
		{
			Name:       "nginx",
			Namespace:  "default",
			Type:       ClusterIPService,
			ClusterIPs: []string{"10.96.0.10"},
			Ports: []Port{
				{Name: "http", Port: 80, Protocol: "TCP"},
				{Name: "https", Port: 443, Protocol: "TCP"},
			},
			TTL: 5,
		},
		{
			Name:       "api-server",
			Namespace:  "production",
			Type:       ClusterIPService,
			ClusterIPs: []string{"10.96.1.100"},
			Ports: []Port{
				{Name: "grpc", Port: 9090, Protocol: "TCP"},
				{Name: "http", Port: 8080, Protocol: "TCP"},
			},
			TTL: 5,
		},
		{
			Name:      "mongodb",
			Namespace: "database",
			Type:      HeadlessService,
			Ports:     []Port{{Name: "mongo", Port: 27017, Protocol: "TCP"}},
			Endpoints: []Endpoint{
				{
					Addresses: []EndpointAddress{
						{IP: "10.244.1.10", Hostname: "mongo-0"},
						{IP: "10.244.1.11", Hostname: "mongo-1"},
						{IP: "10.244.1.12", Hostname: "mongo-2"},
					},
					Ports: []Port{{Name: "mongo", Port: 27017, Protocol: "TCP"}},
				},
			},
			TTL: 5,
		},
		{
			Name:         "external-api",
			Namespace:    "default",
			Type:         ExternalName,
			ExternalName: "api.external-service.com.",
			TTL:          5,
		},
		{
			Name:       "redis",
			Namespace:  "cache",
			Type:       ClusterIPService,
			ClusterIPs: []string{"10.96.2.50"},
			Ports: []Port{
				{Name: "redis", Port: 6379, Protocol: "TCP"},
			},
			TTL: 5,
		},
	}

	for _, svc := range services {
		server.Registry.Register(svc)
		typeStr := "ClusterIP"
		switch svc.Type {
		case HeadlessService:
			typeStr = "Headless"
		case ExternalName:
			typeStr = "ExternalName"
		}
		fmt.Printf("  등록: %s.%s (타입=%s)\n", svc.Name, svc.Namespace, typeStr)
	}
	fmt.Println()

	// ── 데모 1: A 레코드 쿼리 ──
	fmt.Println("── 1. A 레코드 쿼리 (ClusterIP → IP 주소) ──")
	fmt.Println()
	fmt.Println("  DNS 스키마: <service>.<namespace>.svc.cluster.local")
	fmt.Println()

	aQueries := []string{
		"nginx.default.svc.cluster.local.",
		"api-server.production.svc.cluster.local.",
		"redis.cache.svc.cluster.local.",
	}

	for _, q := range aQueries {
		records, rcode := server.Query(q, TypeA)
		if rcode == 0 && len(records) > 0 {
			fmt.Printf("  %-50s → %s %s (TTL=%d)\n",
				q, records[0].TypeString(), records[0].Value, records[0].TTL)
		} else {
			fmt.Printf("  %-50s → NXDOMAIN\n", q)
		}
	}
	fmt.Println()

	// ── 데모 2: Headless 서비스 ──
	fmt.Println("── 2. Headless 서비스 (Pod IP 직접 반환) ──")
	fmt.Println()
	fmt.Println("  Headless 서비스는 ClusterIP 없이 각 Pod의 IP를 직접 반환한다.")
	fmt.Println()

	records, rcode := server.Query("mongodb.database.svc.cluster.local.", TypeA)
	if rcode == 0 {
		for _, r := range records {
			fmt.Printf("  %s → %s %s\n", r.Name, r.TypeString(), r.Value)
		}
	}
	fmt.Println()

	// ── 데모 3: SRV 레코드 ──
	fmt.Println("── 3. SRV 레코드 (포트 정보 포함) ──")
	fmt.Println()
	fmt.Println("  SRV 형식: _port._protocol.service.namespace.svc.zone")
	fmt.Println()

	srvQueries := []struct {
		name string
		desc string
	}{
		{"nginx.default.svc.cluster.local.", "nginx 전체 포트"},
		{"api-server.production.svc.cluster.local.", "api-server 전체 포트"},
		{"mongodb.database.svc.cluster.local.", "mongodb (Headless)"},
	}

	for _, q := range srvQueries {
		records, rcode := server.Query(q.name, TypeSRV)
		fmt.Printf("  %s:\n", q.desc)
		if rcode == 0 {
			for _, r := range records {
				fmt.Printf("    SRV → %s:%d (target=%s)\n", r.Value, r.Port, r.Name)
			}
		} else {
			fmt.Printf("    NXDOMAIN\n")
		}
	}
	fmt.Println()

	// ── 데모 4: ExternalName 서비스 ──
	fmt.Println("── 4. ExternalName 서비스 (CNAME 반환) ──")
	fmt.Println()
	fmt.Println("  ExternalName 서비스는 외부 도메인으로의 CNAME을 반환한다.")
	fmt.Println()

	records, rcode = server.Query("external-api.default.svc.cluster.local.", TypeA)
	if rcode == 0 && len(records) > 0 {
		fmt.Printf("  external-api.default.svc.cluster.local.\n")
		fmt.Printf("    → %s %s\n", records[0].TypeString(), records[0].Value)
	}
	fmt.Println()

	// ── 데모 5: PTR 역방향 조회 ──
	fmt.Println("── 5. PTR 역방향 조회 ──")
	fmt.Println()
	fmt.Println("  ClusterIP로 역방향 조회하면 서비스 FQDN을 반환한다.")
	fmt.Println("  실제 소스: plugin/kubernetes/reverse.go의 serviceRecordForIP()")
	fmt.Println()

	reverseQueries := []string{"10.96.0.10", "10.96.1.100", "10.96.2.50", "10.96.99.99"}
	for _, ip := range reverseQueries {
		rname := ipToReverse(ip)
		records, rcode := server.Query(rname, TypePTR)
		if rcode == 0 && len(records) > 0 {
			fmt.Printf("  %-40s → PTR %s\n", rname, records[0].Value)
		} else {
			fmt.Printf("  %-40s → NXDOMAIN\n", rname)
		}
	}
	fmt.Println()

	// ── 데모 6: DNS 이름 파싱 ──
	fmt.Println("── 6. DNS 이름 파싱 (parseRequest) ──")
	fmt.Println()
	fmt.Println("  실제 소스: plugin/kubernetes/parse.go")
	fmt.Println()

	parseTests := []string{
		"nginx.default.svc.cluster.local.",
		"_http._tcp.nginx.default.svc.cluster.local.",
		"mongo-0.mongodb.database.svc.cluster.local.",
		"10-244-1-10.default.pod.cluster.local.",
	}

	for _, name := range parseTests {
		req, err := parseRequest(name, "cluster.local.")
		if err != nil {
			fmt.Printf("  %-55s → 에러: %v\n", name, err)
			continue
		}
		parts := []string{}
		if req.PodOrSvc != "" {
			parts = append(parts, fmt.Sprintf("type=%s", req.PodOrSvc))
		}
		if req.Namespace != "" {
			parts = append(parts, fmt.Sprintf("ns=%s", req.Namespace))
		}
		if req.Service != "" {
			parts = append(parts, fmt.Sprintf("svc=%s", req.Service))
		}
		if req.Port != "" {
			parts = append(parts, fmt.Sprintf("port=%s", req.Port))
		}
		if req.Protocol != "" {
			parts = append(parts, fmt.Sprintf("proto=%s", req.Protocol))
		}
		if req.Endpoint != "" {
			parts = append(parts, fmt.Sprintf("ep=%s", req.Endpoint))
		}
		fmt.Printf("  %-55s\n    → %s\n", name, strings.Join(parts, ", "))
	}
	fmt.Println()

	// ── 데모 7: 존재하지 않는 서비스/네임스페이스 ──
	fmt.Println("── 7. NXDOMAIN 처리 ──")
	fmt.Println()

	nxQueries := []struct {
		name string
		desc string
	}{
		{"missing.default.svc.cluster.local.", "존재하지 않는 서비스"},
		{"nginx.nonexistent.svc.cluster.local.", "존재하지 않는 네임스페이스"},
	}

	for _, q := range nxQueries {
		_, rcode := server.Query(q.name, TypeA)
		status := "NOERROR"
		if rcode == 3 {
			status = "NXDOMAIN"
		}
		fmt.Printf("  %-50s → %s (%s)\n", q.name, status, q.desc)
	}
	fmt.Println()

	// ── 데모 8: 엔드포인트 호스트명 생성 ──
	fmt.Println("── 8. 엔드포인트 호스트명 생성 ──")
	fmt.Println()
	fmt.Println("  실제 소스: plugin/kubernetes/kubernetes.go의 endpointHostname()")
	fmt.Println()

	epTests := []EndpointAddress{
		{IP: "10.244.1.10", Hostname: "mongo-0"},
		{IP: "10.244.1.11", Hostname: ""},
		{IP: "10.244.2.30", Hostname: ""},
		{IP: "fd00::1:10", Hostname: ""},
	}

	for _, ep := range epTests {
		hostname := endpointHostname(ep)
		if ep.Hostname != "" {
			fmt.Printf("  IP=%-20s Hostname=%-10s → %s (명시적)\n", ep.IP, ep.Hostname, hostname)
		} else {
			fmt.Printf("  IP=%-20s Hostname=%-10s → %s (IP 변환)\n", ep.IP, "(없음)", hostname)
		}
	}
	fmt.Println()

	fmt.Println("=== PoC 완료 ===")
}
