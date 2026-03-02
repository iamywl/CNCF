// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble Flow 데이터 구조와 계층적 프로토콜 모델
//
// Hubble의 Flow는 네트워크 패킷을 L2/L3/L4/L7 계층으로 분리하여 표현합니다.
// 각 계층을 독립적으로 필터링하고 분석할 수 있는 구조입니다.
//
// 실행: go run main.go

package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// ========================================
// 1. Hubble Flow 데이터 모델 (Go 구조체 버전)
// ========================================

// Endpoint는 네트워크 통신의 한쪽 끝점입니다.
// IP 대신 Kubernetes 메타데이터로 표현하는 것이 핵심입니다.
//
// 왜 IP가 아닌 Identity 기반인가?
//   - Pod IP는 동적으로 변함 (재시작하면 다른 IP)
//   - Identity는 같은 레이블 셋을 가진 Pod 그룹에 할당
//   - "10.0.0.5"보다 "default/frontend (app=web)"가 훨씬 유용
type Endpoint struct {
	ID        uint32   `json:"id"`
	Identity  uint32   `json:"identity"`
	Namespace string   `json:"namespace"`
	PodName   string   `json:"pod_name"`
	Labels    []string `json:"labels"`
}

// L2 - Ethernet 계층
type Ethernet struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

// L3 - IP 계층
type IP struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	IPVersion   string `json:"ip_version"` // IPv4 or IPv6
	Encrypted   bool   `json:"encrypted"`  // IPSec/WireGuard
}

// L4 - Transport 계층 (oneof 패턴)
// Protobuf의 oneof를 Go 인터페이스로 시뮬레이션합니다.
//
// 왜 oneof인가?
//   - TCP, UDP, ICMP는 상호 배타적 (하나의 패킷이 동시에 TCP이면서 UDP일 수 없음)
//   - oneof로 타입 안전성 보장
type L4Protocol interface {
	ProtocolName() string
}

type TCP struct {
	SourcePort int  `json:"source_port"`
	DestPort   int  `json:"destination_port"`
	SYN        bool `json:"syn"`
	ACK        bool `json:"ack"`
	FIN        bool `json:"fin"`
	RST        bool `json:"rst"`
}

func (t TCP) ProtocolName() string { return "TCP" }

type UDP struct {
	SourcePort int `json:"source_port"`
	DestPort   int `json:"destination_port"`
}

func (u UDP) ProtocolName() string { return "UDP" }

type ICMP struct {
	Type int `json:"type"`
	Code int `json:"code"`
}

func (i ICMP) ProtocolName() string { return "ICMP" }

// L7 - Application 계층 (oneof 패턴)
type L7Protocol interface {
	ProtocolName() string
}

type DNS struct {
	Query    string   `json:"query"`
	RCode    string   `json:"rcode"`
	IPs      []string `json:"ips"`
	QTypes   []string `json:"qtypes"`
}

func (d DNS) ProtocolName() string { return "DNS" }

type HTTP struct {
	Method    string `json:"method"`
	URL       string `json:"url"`
	Code      int    `json:"code"`
	LatencyNs int64  `json:"latency_ns"`
}

func (h HTTP) ProtocolName() string { return "HTTP" }

// Flow는 Hubble의 핵심 데이터 구조입니다.
// 하나의 네트워크 이벤트의 전체 컨텍스트를 계층적으로 담고 있습니다.
//
// 왜 이렇게 설계되었나?
//   - 계층적 분리: L2/L3/L4/L7을 별도 필드로 분리 → 각 계층 독립 필터링
//   - 양방향 엔드포인트: source/destination 대칭 → 동일 필터 로직 적용
//   - K8s 메타데이터 내장: IP 대신 Pod/Service 정보 → 즉시 활용 가능
//   - Policy 정보 포함: 어떤 정책이 허용/차단했는지 플로우 레벨에서 확인
type Flow struct {
	Timestamp        time.Time `json:"timestamp"`
	UUID             string    `json:"uuid"`
	Verdict          string    `json:"verdict"`
	DropReason       string    `json:"drop_reason,omitempty"`
	Source           Endpoint  `json:"source"`
	Destination      Endpoint  `json:"destination"`
	Ethernet         Ethernet  `json:"ethernet"`
	IP               IP        `json:"ip"`
	L4               L4Protocol
	L7               L7Protocol
	NodeName         string `json:"node_name"`
	TrafficDirection string `json:"traffic_direction"`
	IsReply          bool   `json:"is_reply"`
	Summary          string `json:"summary"`
}

// ========================================
// 2. Flow 생성 예시들
// ========================================

func createTCPFlow() Flow {
	return Flow{
		Timestamp: time.Now(),
		UUID:      "flow-001-tcp",
		Verdict:   "FORWARDED",
		Source: Endpoint{
			ID: 1234, Identity: 56789,
			Namespace: "default", PodName: "frontend-7d8f9c6b5-x2k4n",
			Labels: []string{"app=frontend", "version=v2"},
		},
		Destination: Endpoint{
			ID: 5678, Identity: 12345,
			Namespace: "default", PodName: "backend-api-5c9d8f7a2-j3m7p",
			Labels: []string{"app=backend", "tier=api"},
		},
		Ethernet: Ethernet{Source: "0a:58:0a:f4:00:05", Destination: "0a:58:0a:f4:00:0a"},
		IP:       IP{Source: "10.244.0.5", Destination: "10.244.0.10", IPVersion: "IPv4"},
		L4:       TCP{SourcePort: 52918, DestPort: 8080, SYN: true},
		NodeName: "worker-node-1", TrafficDirection: "EGRESS",
		Summary: "TCP SYN default/frontend → default/backend-api:8080",
	}
}

func createDNSFlow() Flow {
	return Flow{
		Timestamp: time.Now(),
		UUID:      "flow-002-dns",
		Verdict:   "FORWARDED",
		Source: Endpoint{
			ID: 1234, Identity: 56789,
			Namespace: "default", PodName: "frontend-7d8f9c6b5-x2k4n",
			Labels: []string{"app=frontend"},
		},
		Destination: Endpoint{
			ID: 100, Identity: 1,
			Namespace: "kube-system", PodName: "coredns-5d78c9869d-abc12",
			Labels: []string{"k8s-app=kube-dns"},
		},
		Ethernet: Ethernet{Source: "0a:58:0a:f4:00:05", Destination: "0a:58:0a:f4:00:01"},
		IP:       IP{Source: "10.244.0.5", Destination: "10.96.0.10", IPVersion: "IPv4"},
		L4:       UDP{SourcePort: 45123, DestPort: 53},
		L7: DNS{
			Query:  "backend-api.default.svc.cluster.local",
			RCode:  "NOERROR",
			IPs:    []string{"10.244.0.10"},
			QTypes: []string{"A"},
		},
		NodeName: "worker-node-1", TrafficDirection: "EGRESS",
		Summary: "DNS A backend-api.default.svc.cluster.local → 10.244.0.10",
	}
}

func createDroppedFlow() Flow {
	return Flow{
		Timestamp: time.Now(),
		UUID:      "flow-003-drop",
		Verdict:   "DROPPED",
		DropReason: "POLICY_DENIED",
		Source: Endpoint{
			ID: 9999, Identity: 11111,
			Namespace: "untrusted", PodName: "suspicious-pod-abc123",
			Labels: []string{"app=unknown"},
		},
		Destination: Endpoint{
			ID: 5678, Identity: 12345,
			Namespace: "default", PodName: "database-0",
			Labels: []string{"app=database", "tier=data"},
		},
		Ethernet: Ethernet{Source: "0a:58:0a:f4:01:ff", Destination: "0a:58:0a:f4:00:0b"},
		IP:       IP{Source: "10.244.1.255", Destination: "10.244.0.11", IPVersion: "IPv4"},
		L4:       TCP{SourcePort: 38901, DestPort: 3306, SYN: true},
		NodeName: "worker-node-2", TrafficDirection: "INGRESS",
		Summary: "TCP SYN untrusted/suspicious-pod → default/database:3306 POLICY_DENIED",
	}
}

func createHTTPFlow() Flow {
	return Flow{
		Timestamp: time.Now(),
		UUID:      "flow-004-http",
		Verdict:   "FORWARDED",
		Source: Endpoint{
			ID: 1234, Identity: 56789,
			Namespace: "default", PodName: "frontend-7d8f9c6b5-x2k4n",
			Labels: []string{"app=frontend"},
		},
		Destination: Endpoint{
			ID: 5678, Identity: 12345,
			Namespace: "default", PodName: "backend-api-5c9d8f7a2-j3m7p",
			Labels: []string{"app=backend"},
		},
		Ethernet: Ethernet{Source: "0a:58:0a:f4:00:05", Destination: "0a:58:0a:f4:00:0a"},
		IP:       IP{Source: "10.244.0.5", Destination: "10.244.0.10", IPVersion: "IPv4"},
		L4:       TCP{SourcePort: 52918, DestPort: 8080},
		L7: HTTP{
			Method:    "GET",
			URL:       "/api/v1/users?page=1",
			Code:      200,
			LatencyNs: 15_000_000, // 15ms
		},
		NodeName: "worker-node-1", TrafficDirection: "EGRESS",
		Summary: "HTTP GET /api/v1/users → 200 (15ms)",
	}
}

// ========================================
// 3. 각 계층별 분석 함수
// ========================================

func analyzeFlow(flow Flow) {
	fmt.Printf("── Flow: %s ──\n", flow.UUID)
	fmt.Printf("  Verdict: %s", flow.Verdict)
	if flow.DropReason != "" {
		fmt.Printf(" (reason: %s)", flow.DropReason)
	}
	fmt.Println()

	// 엔드포인트 (K8s 메타데이터)
	fmt.Printf("  Source:      %s/%s (Identity:%d)\n",
		flow.Source.Namespace, flow.Source.PodName, flow.Source.Identity)
	fmt.Printf("  Destination: %s/%s (Identity:%d)\n",
		flow.Destination.Namespace, flow.Destination.PodName, flow.Destination.Identity)

	// L2
	fmt.Printf("  L2 (Ethernet): %s → %s\n", flow.Ethernet.Source, flow.Ethernet.Destination)

	// L3
	fmt.Printf("  L3 (IP):       %s → %s (%s, encrypted=%v)\n",
		flow.IP.Source, flow.IP.Destination, flow.IP.IPVersion, flow.IP.Encrypted)

	// L4 (oneof)
	switch l4 := flow.L4.(type) {
	case TCP:
		flags := ""
		if l4.SYN {
			flags += "SYN "
		}
		if l4.ACK {
			flags += "ACK "
		}
		if l4.FIN {
			flags += "FIN "
		}
		if l4.RST {
			flags += "RST "
		}
		fmt.Printf("  L4 (TCP):      :%d → :%d [%s]\n", l4.SourcePort, l4.DestPort, flags)
	case UDP:
		fmt.Printf("  L4 (UDP):      :%d → :%d\n", l4.SourcePort, l4.DestPort)
	case ICMP:
		fmt.Printf("  L4 (ICMP):     type=%d code=%d\n", l4.Type, l4.Code)
	}

	// L7 (oneof)
	if flow.L7 != nil {
		switch l7 := flow.L7.(type) {
		case DNS:
			fmt.Printf("  L7 (DNS):      query=%s rcode=%s ips=%v\n", l7.Query, l7.RCode, l7.IPs)
		case HTTP:
			fmt.Printf("  L7 (HTTP):     %s %s → %d (%.2fms)\n",
				l7.Method, l7.URL, l7.Code, float64(l7.LatencyNs)/1_000_000)
		}
	}

	fmt.Printf("  Summary:       %s\n", flow.Summary)
	fmt.Println()
}

// ========================================
// 4. JSON 직렬화 (Protobuf 대용)
// ========================================

func showJSON(flow Flow) {
	// L4/L7 인터페이스를 포함한 간이 직렬화
	type jsonFlow struct {
		Timestamp   time.Time `json:"timestamp"`
		UUID        string    `json:"uuid"`
		Verdict     string    `json:"verdict"`
		DropReason  string    `json:"drop_reason,omitempty"`
		Source      Endpoint  `json:"source"`
		Destination Endpoint  `json:"destination"`
		IP          IP        `json:"ip"`
		L4Protocol  string    `json:"l4_protocol"`
		L7Protocol  string    `json:"l7_protocol,omitempty"`
		Summary     string    `json:"summary"`
	}

	jf := jsonFlow{
		Timestamp: flow.Timestamp, UUID: flow.UUID,
		Verdict: flow.Verdict, DropReason: flow.DropReason,
		Source: flow.Source, Destination: flow.Destination,
		IP: flow.IP, L4Protocol: flow.L4.ProtocolName(),
		Summary: flow.Summary,
	}
	if flow.L7 != nil {
		jf.L7Protocol = flow.L7.ProtocolName()
	}

	data, _ := json.MarshalIndent(jf, "  ", "  ")
	fmt.Printf("  %s\n", data)
}

func main() {
	fmt.Println("=== PoC: Hubble Flow 데이터 구조 ===")
	fmt.Println()
	fmt.Println("Hubble Flow는 네트워크 패킷을 계층적으로 표현합니다:")
	fmt.Println("  L2 (Ethernet) → L3 (IP) → L4 (TCP/UDP/ICMP) → L7 (DNS/HTTP)")
	fmt.Println("  + Kubernetes 메타데이터 (Pod, Namespace, Labels, Identity)")
	fmt.Println("  + 정책 판정 결과 (Verdict, DropReason)")
	fmt.Println()

	// 4가지 시나리오의 Flow 생성 및 분석
	flows := []struct {
		name string
		flow Flow
	}{
		{"TCP 연결 (SYN) - frontend → backend", createTCPFlow()},
		{"DNS 조회 - frontend → coredns", createDNSFlow()},
		{"정책 차단 - untrusted → database", createDroppedFlow()},
		{"HTTP 요청 - frontend → backend API", createHTTPFlow()},
	}

	fmt.Println("=== 시나리오별 Flow 분석 ===")
	fmt.Println()

	for i, f := range flows {
		fmt.Printf("━━━ 시나리오 %d: %s ━━━\n", i+1, f.name)
		analyzeFlow(f.flow)
	}

	fmt.Println("=== JSON 직렬화 (hubble observe -o json) ===")
	fmt.Println()
	showJSON(createHTTPFlow())

	fmt.Println()
	fmt.Println("-------------------------------------------")
	fmt.Println("핵심 포인트:")
	fmt.Println("  - 계층 분리: L2/L3/L4/L7 독립 필터링 가능")
	fmt.Println("  - oneof 패턴: L4는 TCP|UDP|ICMP 중 하나, L7은 DNS|HTTP 중 하나")
	fmt.Println("  - K8s enrichment: IP 대신 Pod/Namespace/Label로 식별")
	fmt.Println("  - Verdict: FORWARDED/DROPPED/REDIRECTED 정책 결과 포함")
}
