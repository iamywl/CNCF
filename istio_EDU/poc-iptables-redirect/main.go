package main

import (
	"fmt"
	"strings"
)

// =============================================================================
// Istio iptables 트래픽 인터셉션 규칙 생성 시뮬레이션
// =============================================================================
//
// 이 PoC는 Istio가 사이드카(Envoy) 또는 ztunnel로 트래픽을 리다이렉트하기 위해
// 생성하는 iptables 규칙의 구조와 로직을 시뮬레이션한다.
//
// 실제 소스 참조:
//   - istio/tools/istio-iptables/pkg/builder/iptables_builder_impl.go (Rule, IptablesRuleBuilder)
//   - istio/tools/istio-iptables/pkg/capture/run.go (handleInboundPortsInclude, Run)
//   - istio/tools/istio-iptables/pkg/constants/constants.go (체인 이름, 포트 상수)
//   - istio/cni/pkg/iptables/iptables.go (AppendInpodRules - ambient 모드)
//
// 핵심 체인 모델:
//   PREROUTING → ISTIO_INBOUND → ISTIO_IN_REDIRECT → port 15006 (인바운드)
//   OUTPUT → ISTIO_OUTPUT → ISTIO_REDIRECT → port 15001 (아웃바운드)
//
// UID 1337 (envoy 프로세스)의 트래픽은 무한 루프 방지를 위해 스킵한다.
// =============================================================================

// --- 상수 정의 (istio/tools/istio-iptables/pkg/constants/constants.go 기반) ---

const (
	// 사이드카 모드 체인 이름
	ChainISTIO_OUTPUT      = "ISTIO_OUTPUT"
	ChainISTIO_INBOUND     = "ISTIO_INBOUND"
	ChainISTIO_REDIRECT    = "ISTIO_REDIRECT"
	ChainISTIO_IN_REDIRECT = "ISTIO_IN_REDIRECT"
	ChainISTIO_DIVERT      = "ISTIO_DIVERT"
	ChainISTIO_TPROXY      = "ISTIO_TPROXY"

	// 빌트인 체인
	ChainPREROUTING  = "PREROUTING"
	ChainOUTPUT      = "OUTPUT"
	ChainPOSTROUTING = "POSTROUTING"

	// Envoy 포트
	EnvoyOutboundPort     = "15001" // Envoy 아웃바운드 리스너
	EnvoyInboundPort      = "15006" // Envoy 인바운드 리스너
	EnvoyInboundTunnel    = "15008" // HBONE 터널 포트
	DNSCapturePort        = "15053" // DNS 캡처 포트
	EnvoyPrometheusPort   = "15090" // Prometheus 포트
	EnvoyAdminPort        = "15000" // 관리 포트
	EnvoyHealthcheckPort  = "15021" // 헬스체크 포트

	// 프록시 UID/GID (istio-proxy 사용자)
	DefaultProxyUID = "1337"
	DefaultProxyGID = "1337"
)

// 빌트인 체인/타겟 목록 (체인 생성을 건너뛸 대상)
var builtInChainsAndTargets = map[string]bool{
	"INPUT": true, "OUTPUT": true, "FORWARD": true,
	"PREROUTING": true, "POSTROUTING": true,
	"ACCEPT": true, "RETURN": true, "DROP": true,
	"REDIRECT": true, "MARK": true, "TPROXY": true,
	"SNAT": true, "DNAT": true, "CONNMARK": true,
	"CT": true,
}

// --- Rule 구조체 (istio/tools/istio-iptables/pkg/builder/iptables_builder_impl.go 기반) ---

// Rule은 하나의 iptables 규칙을 나타낸다.
// 실제 코드에서는 chain, table, params 필드로 구성된다.
type Rule struct {
	chain  string   // 소속 체인
	table  string   // 테이블 (nat, mangle, raw, filter)
	params []string // 전체 파라미터 (-A chain ... -j TARGET)
}

// String은 iptables 명령 형태로 규칙을 출력한다.
func (r Rule) String() string {
	return fmt.Sprintf("iptables -t %s %s", r.table, strings.Join(r.params, " "))
}

// --- IptablesRuleBuilder (실제 빌더 패턴 재현) ---

// IptablesRuleBuilder는 iptables 규칙을 수집하고 최종 출력을 생성한다.
// 실제 코드: tools/istio-iptables/pkg/builder/iptables_builder_impl.go
type IptablesRuleBuilder struct {
	rules []Rule
}

// NewIptablesRuleBuilder는 새 빌더를 생성한다.
func NewIptablesRuleBuilder() *IptablesRuleBuilder {
	return &IptablesRuleBuilder{
		rules: []Rule{},
	}
}

// AppendRule은 -A (append) 규칙을 추가한다.
// 실제 코드의 appendInternal 함수와 동일한 로직이다.
func (rb *IptablesRuleBuilder) AppendRule(chain, table string, params ...string) {
	fullParams := append([]string{"-A", chain}, params...)
	rb.rules = append(rb.rules, Rule{
		chain:  chain,
		table:  table,
		params: fullParams,
	})
}

// InsertRule은 -I (insert) 규칙을 지정된 위치에 추가한다.
func (rb *IptablesRuleBuilder) InsertRule(chain, table string, position int, params ...string) {
	fullParams := append([]string{"-I", chain, fmt.Sprint(position)}, params...)
	rb.rules = append(rb.rules, Rule{
		chain:  chain,
		table:  table,
		params: fullParams,
	})
}

// BuildRestore는 iptables-restore 형식으로 규칙을 출력한다.
// 실제 코드: BuildV4Restore/BuildV6Restore
// 테이블별로 그룹화하고 체인 생성 명령을 자동으로 삽입한다.
func (rb *IptablesRuleBuilder) BuildRestore() string {
	// 테이블별 규칙 그룹화
	tableRules := make(map[string][]Rule)
	tableOrder := []string{}
	for _, r := range rb.rules {
		if _, exists := tableRules[r.table]; !exists {
			tableOrder = append(tableOrder, r.table)
		}
		tableRules[r.table] = append(tableRules[r.table], r)
	}

	var sb strings.Builder
	for _, table := range tableOrder {
		rules := tableRules[table]
		sb.WriteString(fmt.Sprintf("* %s\n", table))

		// 커스텀 체인 생성 명령 (-N)
		// 빌트인 체인은 건너뛴다.
		createdChains := make(map[string]bool)
		for _, r := range rules {
			if !builtInChainsAndTargets[r.chain] && !createdChains[r.chain] {
				sb.WriteString(fmt.Sprintf(":%s - [0:0]\n", r.chain))
				createdChains[r.chain] = true
			}
		}

		// 규칙 출력
		for _, r := range rules {
			sb.WriteString(strings.Join(r.params, " "))
			sb.WriteString("\n")
		}

		sb.WriteString("COMMIT\n\n")
	}
	return sb.String()
}

// BuildCommands는 개별 iptables 명령 형태로 규칙을 출력한다.
func (rb *IptablesRuleBuilder) BuildCommands() []string {
	// 먼저 커스텀 체인 생성 명령을 수집
	createdChains := make(map[string]bool)
	var cmds []string

	for _, r := range rb.rules {
		chainTable := r.chain + ":" + r.table
		if !builtInChainsAndTargets[r.chain] && !createdChains[chainTable] {
			cmds = append(cmds, fmt.Sprintf("iptables -t %s -N %s", r.table, r.chain))
			createdChains[chainTable] = true
		}
	}

	// 규칙 추가 명령
	for _, r := range rb.rules {
		cmds = append(cmds, r.String())
	}
	return cmds
}

// --- IptablesConfig: 설정 구조체 ---

// IptablesConfig는 iptables 규칙 생성에 필요한 설정을 담는다.
type IptablesConfig struct {
	ProxyPort       string // Envoy 아웃바운드 리스너 포트 (기본 15001)
	InboundPort     string // Envoy 인바운드 리스너 포트 (기본 15006)
	InboundTunnel   string // HBONE 터널 포트 (기본 15008)
	ProxyUID        string // istio-proxy 사용자 UID (기본 1337)
	ProxyGID        string // istio-proxy 사용자 GID (기본 1337)

	// 인바운드 설정
	InboundPortsInclude string // 인터셉트할 인바운드 포트 ("*" 또는 "80,443")
	InboundPortsExclude string // 제외할 인바운드 포트 ("15090,15021")

	// 아웃바운드 설정
	OutboundPortsExclude string // 제외할 아웃바운드 포트
	OutboundCIDRInclude  string // 인터셉트할 CIDR ("*" 또는 "10.0.0.0/8")
	OutboundCIDRExclude  string // 제외할 CIDR

	// DNS 캡처
	RedirectDNS    bool   // DNS 캡처 활성화 여부
	DNSCapturePort string // DNS 캡처 포트 (기본 15053)
	CaptureAllDNS  bool   // 모든 DNS 캡처 여부

	// 인터셉션 모드
	InterceptionMode string // "REDIRECT" 또는 "TPROXY"
}

// DefaultConfig는 기본 설정을 반환한다.
func DefaultConfig() *IptablesConfig {
	return &IptablesConfig{
		ProxyPort:           EnvoyOutboundPort,
		InboundPort:         EnvoyInboundPort,
		InboundTunnel:       EnvoyInboundTunnel,
		ProxyUID:            DefaultProxyUID,
		ProxyGID:            DefaultProxyGID,
		InboundPortsInclude: "*",
		InboundPortsExclude: "15090,15021,15020",
		OutboundCIDRInclude: "*",
		RedirectDNS:         true,
		DNSCapturePort:      DNSCapturePort,
		CaptureAllDNS:       true,
		InterceptionMode:    "REDIRECT",
	}
}

// --- IptablesConfigurator: 규칙 생성기 ---

// IptablesConfigurator는 설정에 따라 iptables 규칙을 생성한다.
// 실제 코드: tools/istio-iptables/pkg/capture/run.go의 IptablesConfigurator
type IptablesConfigurator struct {
	cfg     *IptablesConfig
	builder *IptablesRuleBuilder
}

// NewConfigurator는 새 설정기를 생성한다.
func NewConfigurator(cfg *IptablesConfig) *IptablesConfigurator {
	return &IptablesConfigurator{
		cfg:     cfg,
		builder: NewIptablesRuleBuilder(),
	}
}

// Run은 전체 iptables 규칙을 생성한다.
// 실제 코드: capture/run.go의 Run() 함수 흐름을 재현한다.
func (c *IptablesConfigurator) Run() *IptablesRuleBuilder {
	// 1단계: 인바운드 리다이렉트 체인 설정
	c.setupInboundRedirectChain()

	// 2단계: 아웃바운드 리다이렉트 체인 설정
	c.setupOutboundRedirectChain()

	// 3단계: 인바운드 포트 인터셉션 설정
	c.handleInboundPortsInclude()

	// 4단계: OUTPUT 체인에서 ISTIO_OUTPUT으로 점프
	c.builder.AppendRule(ChainOUTPUT, "nat",
		"-p", "tcp", "-j", ChainISTIO_OUTPUT)

	// 5단계: 아웃바운드 포트 제외
	c.handleOutboundPortsExclude()

	// 6단계: 프록시 UID/GID 트래픽 스킵 (무한루프 방지)
	c.handleProxyUIDSkip()

	// 7단계: DNS 캡처 규칙
	if c.cfg.RedirectDNS {
		c.setupDNSCapture()
	}

	// 8단계: 아웃바운드 CIDR 인클루전/익스클루전
	c.handleOutboundCIDR()

	return c.builder
}

// setupInboundRedirectChain은 인바운드 리다이렉트 체인을 설정한다.
// 실제 코드: ISTIOINREDIRECT 체인은 인바운드 트래픽을 Envoy 인바운드 포트로 REDIRECT한다.
func (c *IptablesConfigurator) setupInboundRedirectChain() {
	// ISTIO_IN_REDIRECT: 인바운드 캡처 포트로 리다이렉트
	c.builder.AppendRule(ChainISTIO_IN_REDIRECT, "nat",
		"-p", "tcp", "-j", "REDIRECT", "--to-ports", c.cfg.InboundPort)
}

// setupOutboundRedirectChain은 아웃바운드 리다이렉트 체인을 설정한다.
// 실제 코드: ISTIOREDIRECT 체인은 아웃바운드 트래픽을 Envoy 아웃바운드 포트로 REDIRECT한다.
func (c *IptablesConfigurator) setupOutboundRedirectChain() {
	// ISTIO_REDIRECT: 프록시 포트로 리다이렉트
	c.builder.AppendRule(ChainISTIO_REDIRECT, "nat",
		"-p", "tcp", "-j", "REDIRECT", "--to-ports", c.cfg.ProxyPort)
}

// handleInboundPortsInclude는 인바운드 포트 인터셉션을 설정한다.
// 실제 코드: capture/run.go의 handleInboundPortsInclude() 함수
func (c *IptablesConfigurator) handleInboundPortsInclude() {
	if c.cfg.InboundPortsInclude == "" {
		return
	}

	// PREROUTING에서 ISTIO_INBOUND로 점프
	c.builder.AppendRule(ChainPREROUTING, "nat",
		"-p", "tcp", "-j", ChainISTIO_INBOUND)

	// HBONE 터널 포트는 직접 통과 (Envoy가 직접 수신)
	c.builder.AppendRule(ChainISTIO_INBOUND, "nat",
		"-p", "tcp", "--dport", c.cfg.InboundTunnel, "-j", "RETURN")

	if c.cfg.InboundPortsInclude == "*" {
		// 와일드카드: 제외 포트를 먼저 RETURN 처리
		if c.cfg.InboundPortsExclude != "" {
			for _, port := range splitPorts(c.cfg.InboundPortsExclude) {
				c.builder.AppendRule(ChainISTIO_INBOUND, "nat",
					"-p", "tcp", "--dport", port, "-j", "RETURN")
			}
		}
		// 나머지 모든 인바운드 트래픽을 리다이렉트
		c.builder.AppendRule(ChainISTIO_INBOUND, "nat",
			"-p", "tcp", "-j", ChainISTIO_IN_REDIRECT)
	} else {
		// 특정 포트만 리다이렉트
		for _, port := range splitPorts(c.cfg.InboundPortsInclude) {
			c.builder.AppendRule(ChainISTIO_INBOUND, "nat",
				"-p", "tcp", "--dport", port, "-j", ChainISTIO_IN_REDIRECT)
		}
	}
}

// handleOutboundPortsExclude는 아웃바운드 포트 제외를 처리한다.
func (c *IptablesConfigurator) handleOutboundPortsExclude() {
	if c.cfg.OutboundPortsExclude == "" {
		return
	}
	for _, port := range splitPorts(c.cfg.OutboundPortsExclude) {
		c.builder.AppendRule(ChainISTIO_OUTPUT, "nat",
			"-p", "tcp", "--dport", port, "-j", "RETURN")
	}
}

// handleProxyUIDSkip은 프록시 프로세스(UID 1337)의 트래픽을 스킵한다.
// 이것이 없으면 Envoy가 보낸 아웃바운드 트래픽이 다시 Envoy로 리다이렉트되어
// 무한 루프가 발생한다. Istio의 핵심 설계 요소이다.
//
// 실제 코드: capture/run.go의 Run() 함수 내 프록시 UID 처리 부분
func (c *IptablesConfigurator) handleProxyUIDSkip() {
	// 127.0.0.6은 인바운드 패스스루 클러스터의 바인드 커넥트에서 사용
	c.builder.AppendRule(ChainISTIO_OUTPUT, "nat",
		"-o", "lo", "-s", "127.0.0.6/32", "-j", "RETURN")

	// 프록시 UID의 루프백 트래픽: 서비스 VIP를 통한 자기 호출은 인바운드로 리다이렉트
	// (appN -> Envoy client -> Envoy server -> appN 패턴)
	if c.cfg.RedirectDNS {
		c.builder.AppendRule(ChainISTIO_OUTPUT, "nat",
			"-o", "lo", "!", "-d", "127.0.0.1/32",
			"-p", "tcp",
			"-m", "multiport", "!", "--dports", "53,"+c.cfg.InboundTunnel,
			"-m", "owner", "--uid-owner", c.cfg.ProxyUID,
			"-j", ChainISTIO_IN_REDIRECT)
	} else {
		c.builder.AppendRule(ChainISTIO_OUTPUT, "nat",
			"-o", "lo", "!", "-d", "127.0.0.1/32",
			"-p", "tcp", "!", "--dport", c.cfg.InboundTunnel,
			"-m", "owner", "--uid-owner", c.cfg.ProxyUID,
			"-j", ChainISTIO_IN_REDIRECT)
	}

	// 루프백의 비프록시 트래픽은 통과 (앱이 자기 자신을 직접 호출하는 경우)
	if c.cfg.RedirectDNS {
		c.builder.AppendRule(ChainISTIO_OUTPUT, "nat",
			"-o", "lo", "-p", "tcp",
			"!", "--dport", "53",
			"-m", "owner", "!", "--uid-owner", c.cfg.ProxyUID,
			"-j", "RETURN")
	} else {
		c.builder.AppendRule(ChainISTIO_OUTPUT, "nat",
			"-o", "lo",
			"-m", "owner", "!", "--uid-owner", c.cfg.ProxyUID,
			"-j", "RETURN")
	}

	// *** 핵심: 프록시 UID의 비루프백 트래픽은 모두 RETURN (무한 루프 방지) ***
	// Envoy가 외부로 보내는 트래픽이 다시 Envoy로 리다이렉트되지 않도록 한다.
	c.builder.AppendRule(ChainISTIO_OUTPUT, "nat",
		"-m", "owner", "--uid-owner", c.cfg.ProxyUID, "-j", "RETURN")

	// GID에 대해서도 동일하게 적용 (프록시가 다른 UID로 실행될 수 있으므로)
	c.builder.AppendRule(ChainISTIO_OUTPUT, "nat",
		"-m", "owner", "--gid-owner", c.cfg.ProxyGID, "-j", "RETURN")
}

// setupDNSCapture는 DNS 캡처 규칙을 설정한다.
// 실제 코드: capture/run.go의 SetupDNSRedir 함수
func (c *IptablesConfigurator) setupDNSCapture() {
	if c.cfg.CaptureAllDNS {
		// 모든 DNS 트래픽 캡처 (UDP)
		c.builder.AppendRule(ChainISTIO_OUTPUT, "nat",
			"-p", "udp", "--dport", "53",
			"-m", "owner", "!", "--uid-owner", c.cfg.ProxyUID,
			"-m", "owner", "!", "--gid-owner", c.cfg.ProxyGID,
			"-j", "REDIRECT", "--to-port", c.cfg.DNSCapturePort)

		// 모든 DNS 트래픽 캡처 (TCP)
		c.builder.AppendRule(ChainISTIO_OUTPUT, "nat",
			"-p", "tcp", "--dport", "53",
			"-m", "owner", "!", "--uid-owner", c.cfg.ProxyUID,
			"-m", "owner", "!", "--gid-owner", c.cfg.ProxyGID,
			"-j", "REDIRECT", "--to-port", c.cfg.DNSCapturePort)

		// DNS conntrack 존 분리 (포트 충돌 방지)
		// 실제 코드: 프록시->업스트림 DNS 존 분리
		c.builder.AppendRule(ChainISTIO_OUTPUT, "raw",
			"-p", "udp", "--dport", "53",
			"-m", "owner", "--uid-owner", c.cfg.ProxyUID,
			"-j", "CT", "--zone", "1")
		c.builder.AppendRule(ChainPREROUTING, "raw",
			"-p", "udp", "--sport", "53",
			"-j", "CT", "--zone", "1")
	}
}

// handleOutboundCIDR은 아웃바운드 CIDR 기반 인터셉션을 처리한다.
func (c *IptablesConfigurator) handleOutboundCIDR() {
	// 로컬호스트 트래픽 스킵
	c.builder.AppendRule(ChainISTIO_OUTPUT, "nat",
		"-d", "127.0.0.1/32", "-j", "RETURN")

	// CIDR 제외
	if c.cfg.OutboundCIDRExclude != "" {
		for _, cidr := range splitPorts(c.cfg.OutboundCIDRExclude) {
			c.builder.AppendRule(ChainISTIO_OUTPUT, "nat",
				"-d", cidr, "-j", "RETURN")
		}
	}

	// CIDR 포함
	if c.cfg.OutboundCIDRInclude == "*" {
		// 와일드카드: 나머지 모든 아웃바운드 트래픽을 리다이렉트
		c.builder.AppendRule(ChainISTIO_OUTPUT, "nat",
			"-j", ChainISTIO_REDIRECT)
	} else if c.cfg.OutboundCIDRInclude != "" {
		for _, cidr := range splitPorts(c.cfg.OutboundCIDRInclude) {
			c.builder.AppendRule(ChainISTIO_OUTPUT, "nat",
				"-d", cidr, "-j", ChainISTIO_REDIRECT)
		}
	}
}

// --- Ambient 모드 규칙 (ztunnel용) ---

// AmbientConfig는 ambient 모드 설정을 담는다.
type AmbientConfig struct {
	ZtunnelInboundPort          string // ztunnel 인바운드 포트 (15008)
	ZtunnelInboundPlaintextPort string // ztunnel 인바운드 평문 포트 (15006)
	ZtunnelOutboundPort         string // ztunnel 아웃바운드 포트 (15001)
	RedirectDNS                 bool
	InpodMark                   string // 패킷 마크 (0x539/0xfff)
	InpodTProxyMark             string // TPROXY 마크 (0x111/0xfff)
}

// DefaultAmbientConfig는 ambient 모드 기본 설정을 반환한다.
func DefaultAmbientConfig() *AmbientConfig {
	return &AmbientConfig{
		ZtunnelInboundPort:          "15008",
		ZtunnelInboundPlaintextPort: "15006",
		ZtunnelOutboundPort:         "15001",
		RedirectDNS:                 true,
		InpodMark:                   "0x539/0xfff",
		InpodTProxyMark:             "0x111/0xfff",
	}
}

// BuildAmbientInpodRules는 ambient 모드의 인팟 iptables 규칙을 생성한다.
// 실제 코드: cni/pkg/iptables/iptables.go의 AppendInpodRules() 함수
func BuildAmbientInpodRules(cfg *AmbientConfig) *IptablesRuleBuilder {
	rb := NewIptablesRuleBuilder()

	// --- mangle 테이블: PREROUTING → ISTIO_PRERT ---
	rb.AppendRule(ChainPREROUTING, "mangle",
		"-j", "ISTIO_PRERT")
	rb.AppendRule(ChainOUTPUT, "mangle",
		"-j", ChainISTIO_OUTPUT)

	// --- nat 테이블 점프 ---
	rb.AppendRule(ChainOUTPUT, "nat",
		"-j", ChainISTIO_OUTPUT)
	rb.AppendRule(ChainPREROUTING, "nat",
		"-j", "ISTIO_PRERT")

	// --- DNS 캡처 (raw 테이블) ---
	if cfg.RedirectDNS {
		rb.AppendRule(ChainPREROUTING, "raw",
			"-j", "ISTIO_PRERT")
		rb.AppendRule(ChainOUTPUT, "raw",
			"-j", ChainISTIO_OUTPUT)
	}

	// --- ISTIO_PRERT 체인 규칙 ---

	// 마크가 있으면 connmark 설정
	rb.AppendRule("ISTIO_PRERT", "mangle",
		"-m", "mark", "--mark", cfg.InpodMark,
		"-j", "CONNMARK", "--set-xmark", cfg.InpodTProxyMark)

	// 로컬호스트가 아니고 마크가 없으면 ztunnel 인바운드 평문 포트로 리다이렉트
	rb.AppendRule("ISTIO_PRERT", "nat",
		"!", "-d", "127.0.0.1/32",
		"-p", "tcp",
		"!", "--dport", cfg.ZtunnelInboundPort,
		"-m", "mark", "!", "--mark", cfg.InpodMark,
		"-j", "REDIRECT", "--to-ports", cfg.ZtunnelInboundPlaintextPort)

	// --- ISTIO_OUTPUT 체인 규칙 ---

	// connmark 복원
	rb.AppendRule(ChainISTIO_OUTPUT, "mangle",
		"-m", "connmark", "--mark", cfg.InpodTProxyMark,
		"-j", "CONNMARK", "--restore-mark",
		"--nfmask", "0xffffffff", "--ctmask", "0xffffffff")

	// DNS 캡처
	if cfg.RedirectDNS {
		rb.AppendRule(ChainISTIO_OUTPUT, "nat",
			"!", "-o", "lo",
			"-p", "udp",
			"-m", "mark", "!", "--mark", cfg.InpodMark,
			"-m", "udp", "--dport", "53",
			"-j", "REDIRECT", "--to-port", DNSCapturePort)

		// DNS conntrack 존 분리
		rb.AppendRule(ChainISTIO_OUTPUT, "raw",
			"-p", "udp",
			"-m", "mark", "--mark", cfg.InpodMark,
			"-m", "udp", "--dport", "53",
			"-j", "CT", "--zone", "1")
		rb.AppendRule("ISTIO_PRERT", "raw",
			"-p", "udp",
			"-m", "mark", "!", "--mark", cfg.InpodMark,
			"-m", "udp", "--sport", "53",
			"-j", "CT", "--zone", "1")
	}

	// 마크가 있으면 통과 (ztunnel이 보낸 트래픽)
	rb.AppendRule(ChainISTIO_OUTPUT, "nat",
		"-p", "tcp",
		"-m", "mark", "--mark", cfg.InpodTProxyMark,
		"-j", "ACCEPT")

	// 로컬호스트가 아니고 마크가 없으면 ztunnel 아웃바운드 포트로 리다이렉트
	rb.AppendRule(ChainISTIO_OUTPUT, "nat",
		"!", "-d", "127.0.0.1/32",
		"-p", "tcp",
		"-m", "mark", "!", "--mark", cfg.InpodMark,
		"-j", "REDIRECT", "--to-ports", cfg.ZtunnelOutboundPort)

	return rb
}

// --- 유틸리티 ---

func splitPorts(s string) []string {
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 80))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 80))
	fmt.Println()
}

// --- main ---

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║       Istio iptables 트래픽 인터셉션 규칙 생성 시뮬레이션           ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	// ==========================================================================
	// 1. 사이드카 모드 (기본 REDIRECT 모드)
	// ==========================================================================
	printSeparator("1. 사이드카 모드 - iptables 규칙 생성")

	cfg := DefaultConfig()
	configurator := NewConfigurator(cfg)
	builder := configurator.Run()

	fmt.Println("--- iptables-restore 형식 출력 ---")
	fmt.Println(builder.BuildRestore())

	// ==========================================================================
	// 2. 트래픽 흐름 시뮬레이션
	// ==========================================================================
	printSeparator("2. 트래픽 흐름 시뮬레이션")

	fmt.Println("[ 인바운드 트래픽 (외부 → 앱) ]")
	fmt.Println()
	fmt.Println("  외부 클라이언트 (10.0.0.1:54321)")
	fmt.Println("       │")
	fmt.Println("       ▼")
	fmt.Println("  PREROUTING (nat)")
	fmt.Println("       │ -j ISTIO_INBOUND")
	fmt.Println("       ▼")
	fmt.Println("  ISTIO_INBOUND (nat)")
	fmt.Println("       │ port 15008? → RETURN (HBONE 터널 직접)")
	fmt.Println("       │ port 15090? → RETURN (Prometheus 제외)")
	fmt.Println("       │ port 15021? → RETURN (헬스체크 제외)")
	fmt.Println("       │ 나머지: -j ISTIO_IN_REDIRECT")
	fmt.Println("       ▼")
	fmt.Println("  ISTIO_IN_REDIRECT (nat)")
	fmt.Println("       │ -j REDIRECT --to-ports 15006")
	fmt.Println("       ▼")
	fmt.Println("  Envoy 인바운드 리스너 (:15006)")
	fmt.Println("       │")
	fmt.Println("       ▼")
	fmt.Println("  앱 컨테이너 (:80)")
	fmt.Println()

	fmt.Println("[ 아웃바운드 트래픽 (앱 → 외부) ]")
	fmt.Println()
	fmt.Println("  앱 컨테이너 (UID != 1337)")
	fmt.Println("       │")
	fmt.Println("       ▼")
	fmt.Println("  OUTPUT (nat)")
	fmt.Println("       │ -j ISTIO_OUTPUT")
	fmt.Println("       ▼")
	fmt.Println("  ISTIO_OUTPUT (nat)")
	fmt.Println("       │ owner UID 1337? → RETURN (무한루프 방지!)")
	fmt.Println("       │ dst 127.0.0.1?  → RETURN (로컬 스킵)")
	fmt.Println("       │ 나머지: -j ISTIO_REDIRECT")
	fmt.Println("       ▼")
	fmt.Println("  ISTIO_REDIRECT (nat)")
	fmt.Println("       │ -j REDIRECT --to-ports 15001")
	fmt.Println("       ▼")
	fmt.Println("  Envoy 아웃바운드 리스너 (:15001)")
	fmt.Println("       │ (이 트래픽의 owner UID = 1337)")
	fmt.Println("       │ (OUTPUT에서 UID 1337이므로 RETURN → 외부로)")
	fmt.Println("       ▼")
	fmt.Println("  외부 서비스")

	// ==========================================================================
	// 3. 포트 제외 로직 검증
	// ==========================================================================
	printSeparator("3. 포트 제외 로직 검증")

	type portTest struct {
		port     string
		expected string
	}
	tests := []portTest{
		{"80", "REDIRECT → Envoy(:15006)"},
		{"443", "REDIRECT → Envoy(:15006)"},
		{"15090", "RETURN (Prometheus 제외)"},
		{"15021", "RETURN (헬스체크 제외)"},
		{"15020", "RETURN (제외 포트)"},
		{"15008", "RETURN (HBONE 터널 직접)"},
		{"8080", "REDIRECT → Envoy(:15006)"},
	}

	excludePorts := map[string]bool{}
	for _, p := range splitPorts(cfg.InboundPortsExclude) {
		excludePorts[p] = true
	}

	fmt.Printf("  %-12s %-35s %s\n", "포트", "결과", "이유")
	fmt.Printf("  %-12s %-35s %s\n", "────", "──────────────────────────────────", "────────────────────")
	for _, t := range tests {
		var result, reason string
		if t.port == "15008" {
			result = "RETURN"
			reason = "HBONE 터널 포트 (직접 Envoy 수신)"
		} else if excludePorts[t.port] {
			result = "RETURN"
			reason = "InboundPortsExclude에 포함"
		} else {
			result = "REDIRECT → :15006"
			reason = "인바운드 캡처 대상"
		}
		fmt.Printf("  %-12s %-35s %s\n", t.port, result, reason)
	}

	// ==========================================================================
	// 4. Ambient 모드 iptables 규칙
	// ==========================================================================
	printSeparator("4. Ambient 모드 - 인팟(In-Pod) iptables 규칙")

	fmt.Println("Ambient 모드에서는 Envoy 사이드카 대신 ztunnel이 L4 트래픽을 처리한다.")
	fmt.Println("패킷 마크(0x539)로 ztunnel 트래픽을 식별하여 무한 루프를 방지한다.")
	fmt.Println()

	ambientCfg := DefaultAmbientConfig()
	ambientBuilder := BuildAmbientInpodRules(ambientCfg)

	fmt.Println("--- Ambient 모드 iptables-restore 형식 ---")
	fmt.Println(ambientBuilder.BuildRestore())

	// ==========================================================================
	// 5. 사이드카 vs Ambient 비교
	// ==========================================================================
	printSeparator("5. 사이드카 모드 vs Ambient 모드 비교")

	fmt.Println("  ┌──────────────────┬───────────────────────────────┬─────────────────────────────────┐")
	fmt.Println("  │ 항목             │ 사이드카 모드                │ Ambient 모드                    │")
	fmt.Println("  ├──────────────────┼───────────────────────────────┼─────────────────────────────────┤")
	fmt.Println("  │ 프록시           │ Envoy (사이드카 컨테이너)     │ ztunnel (노드 레벨 DaemonSet)   │")
	fmt.Println("  │ 인바운드 포트    │ 15006                        │ 15006 (plaintext)               │")
	fmt.Println("  │ 아웃바운드 포트  │ 15001                        │ 15001                           │")
	fmt.Println("  │ 루프방지 메커니즘│ UID 1337 스킵                │ 패킷 마크 0x539 스킵            │")
	fmt.Println("  │ DNS 캡처         │ istio-agent → Envoy          │ ztunnel DNS 프록시              │")
	fmt.Println("  │ 테이블 사용      │ nat (주로)                   │ nat + mangle + raw              │")
	fmt.Println("  │ connmark 사용    │ 미사용                       │ 사용 (0x111 마크)               │")
	fmt.Println("  └──────────────────┴───────────────────────────────┴─────────────────────────────────┘")

	// ==========================================================================
	// 6. DNS 캡처 규칙 상세
	// ==========================================================================
	printSeparator("6. DNS 캡처 메커니즘 상세")

	fmt.Println("  DNS 캡처는 Istio DNS 프록시로 DNS 쿼리를 리다이렉트한다.")
	fmt.Println("  이를 통해 서비스 디스커버리와 DNS 기반 라우팅이 가능해진다.")
	fmt.Println()
	fmt.Println("  앱 컨테이너 (DNS 요청)")
	fmt.Println("       │ UDP/TCP dst port 53")
	fmt.Println("       ▼")
	fmt.Println("  ISTIO_OUTPUT (nat)")
	fmt.Println("       │ UID != 1337 && GID != 1337?")
	fmt.Println("       │ -j REDIRECT --to-port 15053")
	fmt.Println("       ▼")
	fmt.Println("  Istio DNS 프록시 (:15053)")
	fmt.Println("       │ 메시 내 서비스: 직접 응답")
	fmt.Println("       │ 외부 도메인: 업스트림 DNS 포워딩")
	fmt.Println("       ▼")
	fmt.Println("  업스트림 DNS 서버 (:53)")
	fmt.Println("       │ (이때 UID=1337이므로 캡처 안 됨)")
	fmt.Println()
	fmt.Println("  ** conntrack 존 분리 (raw 테이블) **")
	fmt.Println("  프록시→업스트림 DNS와 앱→프록시 DNS의 conntrack을 분리하여")
	fmt.Println("  동일 포트(53) 사용 시 발생할 수 있는 포트 충돌을 방지한다.")
	fmt.Println("  (참고: https://github.com/istio/istio/issues/33469)")

	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}
