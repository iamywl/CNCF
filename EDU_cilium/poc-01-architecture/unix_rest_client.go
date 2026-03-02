// UNIX 소켓 REST 클라이언트 — cilium-dbg가 daemon에 상태를 조회하는 패턴 재현
//
// Cilium 실제 동작:
//   `cilium-dbg endpoint list` 명령은 UNIX 소켓을 통해
//   daemon의 REST API를 호출하여 Endpoint 정보를 가져온다.
//
// 실행: go run unix_rest_client.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
)

const socketPath = "/tmp/cilium-poc.sock"

func main() {
	fmt.Println("=== cilium-dbg (UNIX 소켓 REST 클라이언트) ===")
	fmt.Println("Cilium 실제 코드: cilium-dbg/")
	fmt.Println()

	// UNIX 소켓으로 HTTP 클라이언트 생성
	// TCP 대신 파일 시스템 소켓으로 연결한다
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}

	// 1) cilium-dbg endpoint list
	fmt.Println(">>> cilium-dbg endpoint list")
	fmt.Println("─────────────────────────────────────────────────────────────")
	callAPI(client, "/v1/endpoint", func(body []byte) {
		var endpoints []map[string]interface{}
		json.Unmarshal(body, &endpoints)
		fmt.Printf("%-6s %-20s %-12s %-10s %-15s %s\n",
			"ID", "POD NAME", "IDENTITY", "STATE", "IPv4", "POLICY")
		for _, ep := range endpoints {
			fmt.Printf("%-6.0f %-20s %-12.0f %-10s %-15s %s\n",
				ep["id"], ep["pod-name"], ep["identity"],
				ep["state"], ep["ipv4"], ep["policy-enabled"])
		}
	})

	fmt.Println()

	// 2) cilium-dbg identity list
	fmt.Println(">>> cilium-dbg identity list")
	fmt.Println("─────────────────────────────────────────────────────────────")
	callAPI(client, "/v1/identity", func(body []byte) {
		var identities []map[string]interface{}
		json.Unmarshal(body, &identities)
		for _, id := range identities {
			fmt.Printf("ID: %-8.0f Labels: %v\n", id["id"], id["labels"])
		}
	})

	fmt.Println()

	// 3) cilium-dbg config
	fmt.Println(">>> cilium-dbg config")
	fmt.Println("─────────────────────────────────────────────────────────────")
	callAPI(client, "/v1/config", func(body []byte) {
		var config map[string]interface{}
		json.Unmarshal(body, &config)
		for k, v := range config {
			fmt.Printf("  %-25s = %v\n", k, v)
		}
	})
}

func callAPI(client *http.Client, path string, printer func([]byte)) {
	// URL의 호스트 부분은 무시됨 — UNIX 소켓이므로
	resp, err := client.Get("http://localhost" + path)
	if err != nil {
		log.Fatalf("요청 실패 (%s): %v", path, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	printer(body)
}
