// UNIX 소켓 REST 서버 — cilium-daemon의 REST API 패턴 재현
//
// Cilium 실제 동작:
//   cilium-daemon은 /var/run/cilium/cilium.sock에 UNIX 소켓으로
//   REST API를 노출한다. cilium-dbg CLI가 이 소켓을 통해 상태를 조회한다.
//
// 실행: go run unix_rest_server.go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
)

const socketPath = "/tmp/cilium-poc.sock"

// Cilium의 Endpoint 모델을 단순화한 구조체
// 실제 코드: pkg/endpoint/endpoint.go
type Endpoint struct {
	ID        uint16 `json:"id"`
	PodName   string `json:"pod-name"`
	Namespace string `json:"namespace"`
	Identity  uint32 `json:"identity"`
	State     string `json:"state"`
	IPv4      string `json:"ipv4"`
	PolicyEnabled string `json:"policy-enabled"`
}

// Cilium의 Identity 모델을 단순화한 구조체
// 실제 코드: pkg/identity/identity.go
type Identity struct {
	ID     uint32            `json:"id"`
	Labels map[string]string `json:"labels"`
}

var endpoints = []Endpoint{
	{ID: 1234, PodName: "frontend-7b4d8", Namespace: "default", Identity: 48312, State: "ready", IPv4: "10.0.1.5", PolicyEnabled: "ingress"},
	{ID: 5678, PodName: "backend-3c9a1", Namespace: "default", Identity: 48313, State: "ready", IPv4: "10.0.1.10", PolicyEnabled: "both"},
	{ID: 9012, PodName: "redis-5f2e7", Namespace: "default", Identity: 48314, State: "regenerating", IPv4: "10.0.1.15", PolicyEnabled: "none"},
}

var identities = []Identity{
	{ID: 1, Labels: map[string]string{"reserved": "host"}},
	{ID: 2, Labels: map[string]string{"reserved": "world"}},
	{ID: 48312, Labels: map[string]string{"app": "frontend", "k8s:io.kubernetes.pod.namespace": "default"}},
	{ID: 48313, Labels: map[string]string{"app": "backend", "k8s:io.kubernetes.pod.namespace": "default"}},
	{ID: 48314, Labels: map[string]string{"app": "redis", "k8s:io.kubernetes.pod.namespace": "default"}},
}

func main() {
	// 기존 소켓 파일 정리
	os.Remove(socketPath)

	mux := http.NewServeMux()

	// /v1/endpoint — Cilium 실제 API: api/v1/openapi.yaml
	mux.HandleFunc("/v1/endpoint", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(endpoints)
	})

	// /v1/identity — Identity 목록
	mux.HandleFunc("/v1/identity", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(identities)
	})

	// /v1/healthz — 헬스 체크
	mux.HandleFunc("/v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// /v1/config — 런타임 설정
	mux.HandleFunc("/v1/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"RoutingMode":          "tunnel",
			"TunnelProtocol":       "vxlan",
			"EnableIPv4":           true,
			"EnableIPv6":           true,
			"EnablePolicy":         "default",
			"KubeProxyReplacement": false,
		})
	})

	// UNIX 소켓에서 리슨 — TCP가 아닌 파일 시스템 소켓
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Fatalf("UNIX 소켓 생성 실패: %v", err)
	}
	defer listener.Close()

	fmt.Println("=== cilium-daemon (UNIX 소켓 REST 서버) ===")
	fmt.Printf("소켓: %s\n", socketPath)
	fmt.Println("Cilium 실제 소켓: /var/run/cilium/cilium.sock")
	fmt.Println()
	fmt.Println("API 엔드포인트:")
	fmt.Println("  GET /v1/endpoint  — Endpoint 목록")
	fmt.Println("  GET /v1/identity  — Identity 목록")
	fmt.Println("  GET /v1/healthz   — 헬스 체크")
	fmt.Println("  GET /v1/config    — 런타임 설정")
	fmt.Println()

	http.Serve(listener, mux)
}
