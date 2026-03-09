// Gateway API / Ingress 시뮬레이션
//
// 이 PoC는 Cilium Gateway API 구현의 핵심 개념을 시뮬레이션한다:
// 1. GatewayClass → Gateway → HTTPRoute 리소스 계층
// 2. Gateway 리컨실레이션 파이프라인
// 3. Ingestion: Gateway API 리소스 → 내부 모델 변환
// 4. Translation: 내부 모델 → CiliumEnvoyConfig + Service 생성
// 5. 라우트 매칭 및 필터링
// 6. 크로스 네임스페이스 ReferenceGrant 검사
//
// 실제 Cilium 구현을 참고:
// - operator/pkg/gateway-api/cell.go (Cell 구조)
// - operator/pkg/gateway-api/gateway.go (Gateway 리컨실러)
// - operator/pkg/gateway-api/gateway_reconcile.go (리컨실 로직)
// - operator/pkg/gateway-api/controller.go (컨트롤러 이름, 유틸)
//
// 실행: go run main.go

package main

import (
	"fmt"
	"strings"
)

// ─── Gateway API 리소스 타입 ───

// GatewayClass는 Kubernetes GatewayClass를 모방
type GatewayClass struct {
	Name           string
	ControllerName string // "io.cilium/gateway-controller"
	Accepted       bool
}

// Listener는 Gateway의 리스너
type Listener struct {
	Name     string
	Protocol string // HTTP, HTTPS, TLS
	Port     int
	Hostname string // 선택적
	TLS      *TLSConfig
	AllowedRoutes AllowedRoutes
}

type TLSConfig struct {
	Mode          string // Terminate, Passthrough
	CertificateRef string // Secret 이름
}

type AllowedRoutes struct {
	Namespaces NamespaceSelector
}

type NamespaceSelector struct {
	From string // All, Same, Selector
}

// Gateway는 Kubernetes Gateway를 모방
type Gateway struct {
	Name             string
	Namespace        string
	GatewayClassName string
	Listeners        []Listener
	Addresses        []string
	// 상태
	Accepted    bool
	Programmed  bool
	StatusMsg   string
}

// HTTPRoute는 Kubernetes HTTPRoute를 모방
type HTTPRoute struct {
	Name       string
	Namespace  string
	ParentRefs []ParentRef
	Hostnames  []string
	Rules      []HTTPRouteRule
}

type ParentRef struct {
	Name      string
	Namespace string // 비어있으면 Route의 네임스페이스 사용
}

type HTTPRouteRule struct {
	Matches    []HTTPRouteMatch
	BackendRefs []BackendRef
}

type HTTPRouteMatch struct {
	PathType  string // Exact, PathPrefix
	PathValue string
	Headers   map[string]string
}

type BackendRef struct {
	ServiceName string
	ServicePort int
	Namespace   string // 비어있으면 Route의 네임스페이스 사용
	Weight      int
}

// ReferenceGrant는 크로스 네임스페이스 참조 허용
type ReferenceGrant struct {
	Name      string
	Namespace string
	From      []ReferenceGrantFrom
	To        []ReferenceGrantTo
}

type ReferenceGrantFrom struct {
	Group     string
	Kind      string
	Namespace string
}

type ReferenceGrantTo struct {
	Group string
	Kind  string
	Name  string
}

// ─── 내부 모델 (Cilium model.Model 참고) ───

type HTTPListener struct {
	Name      string
	Hostname  string
	Port      int
	TLS       bool
	Routes    []HTTPListenerRoute
}

type HTTPListenerRoute struct {
	Hostnames   []string
	PathMatch   string
	Backends    []Backend
}

type Backend struct {
	Name      string
	Namespace string
	Port      int
	Weight    int
}

// ─── 출력 리소스 ───

// CiliumEnvoyConfig는 Envoy 프록시 설정
type CiliumEnvoyConfig struct {
	Name      string
	Namespace string
	Listeners []EnvoyListener
	Routes    []EnvoyRoute
	Clusters  []EnvoyCluster
}

type EnvoyListener struct {
	Name     string
	Port     int
	Protocol string
	TLS      bool
}

type EnvoyRoute struct {
	Domains   []string
	PathMatch string
	Cluster   string
}

type EnvoyCluster struct {
	Name    string
	Service string
	Port    int
}

// LBService는 LoadBalancer Service
type LBService struct {
	Name      string
	Namespace string
	Ports     []ServicePort
	Type      string
}

type ServicePort struct {
	Name     string
	Port     int
	Protocol string
}

// ─── 컨트롤러 ───

const controllerName = "io.cilium/gateway-controller"

// GatewayAPIController는 Gateway API 컨트롤러를 시뮬레이션
type GatewayAPIController struct {
	gatewayClasses  map[string]*GatewayClass
	gateways        map[string]*Gateway        // namespace/name → Gateway
	httpRoutes      map[string]*HTTPRoute       // namespace/name → HTTPRoute
	referenceGrants map[string]*ReferenceGrant  // namespace/name → ReferenceGrant
	cecStore        map[string]*CiliumEnvoyConfig
	svcStore        map[string]*LBService
}

func NewGatewayAPIController() *GatewayAPIController {
	return &GatewayAPIController{
		gatewayClasses:  make(map[string]*GatewayClass),
		gateways:        make(map[string]*Gateway),
		httpRoutes:      make(map[string]*HTTPRoute),
		referenceGrants: make(map[string]*ReferenceGrant),
		cecStore:        make(map[string]*CiliumEnvoyConfig),
		svcStore:        make(map[string]*LBService),
	}
}

// ─── GatewayClass 리컨실 (Cilium gatewayclass_reconcile.go 참고) ───

func (c *GatewayAPIController) ReconcileGatewayClass(gc *GatewayClass) {
	fmt.Printf("  [GatewayClass] 리컨실: %s (controller=%s)\n", gc.Name, gc.ControllerName)

	if gc.ControllerName == controllerName {
		gc.Accepted = true
		fmt.Printf("    → Accepted: true (Cilium 컨트롤러 매칭)\n")
	} else {
		gc.Accepted = false
		fmt.Printf("    → Accepted: false (컨트롤러 불일치)\n")
	}
	c.gatewayClasses[gc.Name] = gc
}

// ─── Gateway 리컨실 (Cilium gateway_reconcile.go 참고) ───

func (c *GatewayAPIController) ReconcileGateway(gw *Gateway) {
	key := gw.Namespace + "/" + gw.Name
	fmt.Printf("\n  [Gateway] 리컨실: %s\n", key)

	// Step 1: GatewayClass 확인
	gc, exists := c.gatewayClasses[gw.GatewayClassName]
	if !exists || !gc.Accepted {
		gw.Accepted = false
		gw.StatusMsg = "GatewayClass not found or not accepted"
		fmt.Printf("    → Accepted: false (%s)\n", gw.StatusMsg)
		c.gateways[key] = gw
		return
	}

	// Step 2: Route 수집 및 필터링
	matchedRoutes := c.filterHTTPRoutesByGateway(gw)
	fmt.Printf("    매칭된 HTTPRoute: %d개\n", len(matchedRoutes))

	// Step 3: Ingestion - Gateway API → 내부 모델
	httpListeners := c.ingest(gw, matchedRoutes)
	fmt.Printf("    Ingestion 결과: HTTPListener %d개\n", len(httpListeners))

	// Step 4: Translation - 내부 모델 → CEC + Service
	cec, svc := c.translate(gw, httpListeners)

	// Step 5: 리소스 생성/업데이트
	c.ensureEnvoyConfig(cec)
	c.ensureService(svc)

	// Step 6: 상태 업데이트
	gw.Accepted = true
	gw.Programmed = true
	gw.StatusMsg = "Gateway programmed"
	gw.Addresses = []string{"198.51.100.1"} // 시뮬레이션: LB-IPAM이 할당한 IP
	c.gateways[key] = gw

	fmt.Printf("    → Accepted: true, Programmed: true\n")
	fmt.Printf("    → Address: %s\n", strings.Join(gw.Addresses, ", "))
}

// filterHTTPRoutesByGateway는 Gateway에 연결된 HTTPRoute를 필터링한다
func (c *GatewayAPIController) filterHTTPRoutesByGateway(gw *Gateway) []*HTTPRoute {
	var matched []*HTTPRoute

	for _, route := range c.httpRoutes {
		for _, parentRef := range route.ParentRefs {
			// 네임스페이스 확인
			ns := parentRef.Namespace
			if ns == "" {
				ns = route.Namespace
			}
			if ns == gw.Namespace && parentRef.Name == gw.Name {
				// 크로스 네임스페이스 확인
				if route.Namespace != gw.Namespace {
					if !c.isReferenceGranted(route.Namespace, gw.Namespace, "Gateway", gw.Name) {
						fmt.Printf("    Route %s/%s: ReferenceGrant 없음 → 건너뜀\n", route.Namespace, route.Name)
						continue
					}
				}

				// 리스너 AllowedRoutes 확인
				allowed := false
				for _, l := range gw.Listeners {
					switch l.AllowedRoutes.Namespaces.From {
					case "All":
						allowed = true
					case "Same":
						allowed = route.Namespace == gw.Namespace
					default:
						allowed = true
					}
					if allowed {
						break
					}
				}

				if allowed {
					matched = append(matched, route)
				}
			}
		}
	}

	return matched
}

// isReferenceGranted는 크로스 네임스페이스 참조가 허용되는지 확인한다
func (c *GatewayAPIController) isReferenceGranted(fromNS, toNS, toKind, toName string) bool {
	for _, grant := range c.referenceGrants {
		if grant.Namespace != toNS {
			continue
		}
		for _, from := range grant.From {
			if from.Namespace == fromNS && from.Kind == "HTTPRoute" {
				for _, to := range grant.To {
					if to.Kind == toKind && (to.Name == "" || to.Name == toName) {
						return true
					}
				}
			}
		}
	}
	return false
}

// ─── Ingestion (Cilium ingestion.GatewayAPI 참고) ───

func (c *GatewayAPIController) ingest(gw *Gateway, routes []*HTTPRoute) []HTTPListener {
	var listeners []HTTPListener

	for _, l := range gw.Listeners {
		listener := HTTPListener{
			Name:     l.Name,
			Hostname: l.Hostname,
			Port:     l.Port,
			TLS:      l.TLS != nil,
		}

		// Route 매칭
		for _, route := range routes {
			for _, rule := range route.Rules {
				lr := HTTPListenerRoute{
					Hostnames: route.Hostnames,
				}

				// 경로 매칭
				if len(rule.Matches) > 0 {
					lr.PathMatch = rule.Matches[0].PathValue
				}

				// 백엔드 해석
				for _, br := range rule.BackendRefs {
					ns := br.Namespace
					if ns == "" {
						ns = route.Namespace
					}
					lr.Backends = append(lr.Backends, Backend{
						Name:      br.ServiceName,
						Namespace: ns,
						Port:      br.ServicePort,
						Weight:    br.Weight,
					})
				}

				listener.Routes = append(listener.Routes, lr)
			}
		}

		listeners = append(listeners, listener)
	}

	return listeners
}

// ─── Translation (Cilium translation.Translate 참고) ───

func (c *GatewayAPIController) translate(gw *Gateway, listeners []HTTPListener) (*CiliumEnvoyConfig, *LBService) {
	cecName := "cilium-gateway-" + gw.Name

	cec := &CiliumEnvoyConfig{
		Name:      cecName,
		Namespace: gw.Namespace,
	}

	svc := &LBService{
		Name:      cecName,
		Namespace: gw.Namespace,
		Type:      "LoadBalancer",
	}

	clusterIdx := 0
	for _, l := range listeners {
		// Envoy 리스너 생성
		cec.Listeners = append(cec.Listeners, EnvoyListener{
			Name:     l.Name,
			Port:     l.Port,
			Protocol: "HTTP",
			TLS:      l.TLS,
		})

		// Service 포트 추가
		svc.Ports = append(svc.Ports, ServicePort{
			Name:     l.Name,
			Port:     l.Port,
			Protocol: "TCP",
		})

		// 각 Route에 대해 Envoy Route + Cluster 생성
		for _, route := range l.Routes {
			for _, backend := range route.Backends {
				clusterName := fmt.Sprintf("%s/%s:%d", backend.Namespace, backend.Name, backend.Port)

				cec.Routes = append(cec.Routes, EnvoyRoute{
					Domains:   route.Hostnames,
					PathMatch: route.PathMatch,
					Cluster:   clusterName,
				})

				cec.Clusters = append(cec.Clusters, EnvoyCluster{
					Name:    clusterName,
					Service: backend.Namespace + "/" + backend.Name,
					Port:    backend.Port,
				})
				clusterIdx++
			}
		}
	}

	return cec, svc
}

func (c *GatewayAPIController) ensureEnvoyConfig(cec *CiliumEnvoyConfig) {
	key := cec.Namespace + "/" + cec.Name
	c.cecStore[key] = cec
	fmt.Printf("    CiliumEnvoyConfig 생성: %s\n", key)
}

func (c *GatewayAPIController) ensureService(svc *LBService) {
	key := svc.Namespace + "/" + svc.Name
	c.svcStore[key] = svc
	fmt.Printf("    LoadBalancer Service 생성: %s (ports: ", key)
	var ports []string
	for _, p := range svc.Ports {
		ports = append(ports, fmt.Sprintf("%s/%d", p.Name, p.Port))
	}
	fmt.Printf("%s)\n", strings.Join(ports, ", "))
}

// ─── 상태 출력 ───

func (c *GatewayAPIController) PrintStatus() {
	fmt.Println("\n  ┌─ GatewayClasses:")
	for _, gc := range c.gatewayClasses {
		fmt.Printf("  │  %s (controller=%s, accepted=%v)\n", gc.Name, gc.ControllerName, gc.Accepted)
	}

	fmt.Println("  ├─ Gateways:")
	for key, gw := range c.gateways {
		fmt.Printf("  │  %s (class=%s, accepted=%v, programmed=%v)\n", key, gw.GatewayClassName, gw.Accepted, gw.Programmed)
		for _, l := range gw.Listeners {
			tls := ""
			if l.TLS != nil {
				tls = fmt.Sprintf(" [TLS:%s]", l.TLS.Mode)
			}
			fmt.Printf("  │    Listener: %s (%s:%d)%s\n", l.Name, l.Protocol, l.Port, tls)
		}
		if len(gw.Addresses) > 0 {
			fmt.Printf("  │    Addresses: %s\n", strings.Join(gw.Addresses, ", "))
		}
	}

	fmt.Println("  ├─ CiliumEnvoyConfigs:")
	for key, cec := range c.cecStore {
		fmt.Printf("  │  %s\n", key)
		for _, l := range cec.Listeners {
			tls := ""
			if l.TLS {
				tls = " [TLS]"
			}
			fmt.Printf("  │    Listener: %s (%s:%d)%s\n", l.Name, l.Protocol, l.Port, tls)
		}
		for _, r := range cec.Routes {
			fmt.Printf("  │    Route: %s %s → %s\n", strings.Join(r.Domains, ","), r.PathMatch, r.Cluster)
		}
	}

	fmt.Println("  ├─ Services:")
	for key, svc := range c.svcStore {
		var ports []string
		for _, p := range svc.Ports {
			ports = append(ports, fmt.Sprintf("%d", p.Port))
		}
		fmt.Printf("  │  %s (type=%s, ports=%s)\n", key, svc.Type, strings.Join(ports, ","))
	}
	fmt.Println("  └─")
}

// ─── 메인 ───

func main() {
	fmt.Println("╔════════════════════════════════════════════════════════╗")
	fmt.Println("║     Cilium Gateway API 시뮬레이션                      ║")
	fmt.Println("╚════════════════════════════════════════════════════════╝")

	ctrl := NewGatewayAPIController()

	// ─── 시나리오 1: GatewayClass 등록 ───
	fmt.Println("\n━━━ 시나리오 1: GatewayClass 등록 ━━━")

	ctrl.ReconcileGatewayClass(&GatewayClass{
		Name:           "cilium",
		ControllerName: controllerName,
	})

	ctrl.ReconcileGatewayClass(&GatewayClass{
		Name:           "nginx",
		ControllerName: "k8s.io/nginx-controller",
	})

	// ─── 시나리오 2: Gateway + HTTPRoute 생성 ───
	fmt.Println("\n━━━ 시나리오 2: Gateway + HTTPRoute 리컨실레이션 ━━━")

	// HTTPRoute 먼저 등록
	ctrl.httpRoutes["default/app-route"] = &HTTPRoute{
		Name:      "app-route",
		Namespace: "default",
		ParentRefs: []ParentRef{
			{Name: "my-gateway"},
		},
		Hostnames: []string{"app.example.com"},
		Rules: []HTTPRouteRule{
			{
				Matches: []HTTPRouteMatch{
					{PathType: "PathPrefix", PathValue: "/api"},
				},
				BackendRefs: []BackendRef{
					{ServiceName: "api-svc", ServicePort: 8080, Weight: 100},
				},
			},
			{
				Matches: []HTTPRouteMatch{
					{PathType: "PathPrefix", PathValue: "/"},
				},
				BackendRefs: []BackendRef{
					{ServiceName: "frontend-svc", ServicePort: 80, Weight: 80},
					{ServiceName: "frontend-svc-v2", ServicePort: 80, Weight: 20},
				},
			},
		},
	}

	// Gateway 리컨실
	ctrl.ReconcileGateway(&Gateway{
		Name:             "my-gateway",
		Namespace:        "default",
		GatewayClassName: "cilium",
		Listeners: []Listener{
			{
				Name:     "http",
				Protocol: "HTTP",
				Port:     80,
				AllowedRoutes: AllowedRoutes{
					Namespaces: NamespaceSelector{From: "All"},
				},
			},
			{
				Name:     "https",
				Protocol: "HTTPS",
				Port:     443,
				TLS:      &TLSConfig{Mode: "Terminate", CertificateRef: "my-tls-cert"},
				AllowedRoutes: AllowedRoutes{
					Namespaces: NamespaceSelector{From: "Same"},
				},
			},
		},
	})

	ctrl.PrintStatus()

	// ─── 시나리오 3: 크로스 네임스페이스 Route ───
	fmt.Println("\n━━━ 시나리오 3: 크로스 네임스페이스 Route (ReferenceGrant 없음) ━━━")

	ctrl.httpRoutes["other-ns/cross-route"] = &HTTPRoute{
		Name:      "cross-route",
		Namespace: "other-ns",
		ParentRefs: []ParentRef{
			{Name: "my-gateway", Namespace: "default"},
		},
		Hostnames: []string{"other.example.com"},
		Rules: []HTTPRouteRule{
			{
				Matches: []HTTPRouteMatch{
					{PathType: "PathPrefix", PathValue: "/"},
				},
				BackendRefs: []BackendRef{
					{ServiceName: "other-svc", ServicePort: 8080, Weight: 100},
				},
			},
		},
	}

	// Gateway 재리컨실 (Route 변경으로 트리거)
	ctrl.ReconcileGateway(&Gateway{
		Name:             "my-gateway",
		Namespace:        "default",
		GatewayClassName: "cilium",
		Listeners: []Listener{
			{
				Name:     "http",
				Protocol: "HTTP",
				Port:     80,
				AllowedRoutes: AllowedRoutes{
					Namespaces: NamespaceSelector{From: "All"},
				},
			},
		},
	})

	// ─── 시나리오 4: ReferenceGrant 추가 ───
	fmt.Println("\n━━━ 시나리오 4: ReferenceGrant 추가 후 재리컨실 ━━━")

	ctrl.referenceGrants["default/allow-other-ns"] = &ReferenceGrant{
		Name:      "allow-other-ns",
		Namespace: "default",
		From: []ReferenceGrantFrom{
			{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Namespace: "other-ns"},
		},
		To: []ReferenceGrantTo{
			{Group: "gateway.networking.k8s.io", Kind: "Gateway"},
		},
	}

	ctrl.ReconcileGateway(&Gateway{
		Name:             "my-gateway",
		Namespace:        "default",
		GatewayClassName: "cilium",
		Listeners: []Listener{
			{
				Name:     "http",
				Protocol: "HTTP",
				Port:     80,
				AllowedRoutes: AllowedRoutes{
					Namespaces: NamespaceSelector{From: "All"},
				},
			},
		},
	})

	ctrl.PrintStatus()

	// ─── 시나리오 5: 잘못된 GatewayClass 참조 ───
	fmt.Println("\n━━━ 시나리오 5: 잘못된 GatewayClass 참조 ━━━")

	ctrl.ReconcileGateway(&Gateway{
		Name:             "bad-gateway",
		Namespace:        "default",
		GatewayClassName: "nginx", // Cilium이 관리하지 않는 클래스
	})

	fmt.Println("\n╔════════════════════════════════════════════════════════╗")
	fmt.Println("║     시뮬레이션 완료                                     ║")
	fmt.Println("╚════════════════════════════════════════════════════════╝")
}
