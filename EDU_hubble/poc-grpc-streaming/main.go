// SPDX-License-Identifier: Apache-2.0
// PoC: gRPC Server Streaming 패턴 시뮬레이션
//
// Hubble의 GetFlows RPC는 Server Streaming 방식입니다.
// 서버가 클라이언트에게 Flow를 연속적으로 보내주는 구조입니다.
//
// 이 PoC는 net 패키지로 gRPC의 server streaming 패턴을 시뮬레이션합니다.
// 실행: go run main.go

package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"
)

// ========================================
// 1. 데이터 모델 (Hubble Flow 간소화 버전)
// ========================================

type Flow struct {
	Timestamp   time.Time `json:"timestamp"`
	Source      string    `json:"source"`
	Destination string    `json:"destination"`
	Verdict     string    `json:"verdict"`
	Protocol    string    `json:"protocol"`
	Port        int       `json:"port"`
}

// ========================================
// 2. Server (Hubble Server 역할)
// ========================================

// handleClient는 Hubble Server의 GetFlows RPC를 시뮬레이션합니다.
// 클라이언트가 연결하면 Flow를 지속적으로 스트리밍합니다.
//
// 실제 Hubble에서는:
//   - eBPF에서 이벤트가 올 때마다 gRPC stream으로 전송
//   - 클라이언트가 --follow 플래그를 사용하면 무한 스트림
//   - 클라이언트가 --last N을 사용하면 N개만 전송 후 종료
func handleClient(conn net.Conn, flowCount int) {
	defer conn.Close()
	encoder := json.NewEncoder(conn)

	pods := []string{"frontend", "backend", "database", "cache", "api-gateway"}
	verdicts := []string{"FORWARDED", "DROPPED", "REDIRECTED"}
	protocols := []string{"TCP", "UDP", "HTTP", "DNS"}

	fmt.Printf("[Server] 클라이언트 연결됨: %s\n", conn.RemoteAddr())
	fmt.Printf("[Server] %d개 Flow 스트리밍 시작...\n", flowCount)

	for i := 0; i < flowCount; i++ {
		flow := Flow{
			Timestamp:   time.Now(),
			Source:      fmt.Sprintf("default/%s", pods[rand.Intn(len(pods))]),
			Destination: fmt.Sprintf("default/%s", pods[rand.Intn(len(pods))]),
			Verdict:     verdicts[rand.Intn(len(verdicts))],
			Protocol:    protocols[rand.Intn(len(protocols))],
			Port:        []int{80, 443, 8080, 53, 3306}[rand.Intn(5)],
		}

		if err := encoder.Encode(flow); err != nil {
			fmt.Printf("[Server] 전송 실패: %v\n", err)
			return
		}

		// eBPF 이벤트가 발생하는 간격을 시뮬레이션
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Println("[Server] 스트리밍 완료")
}

func startServer(addr string, flowCount int, wg *sync.WaitGroup) {
	defer wg.Done()

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Printf("[Server] 리슨 실패: %v\n", err)
		return
	}
	defer listener.Close()

	fmt.Printf("[Server] 포트 %s에서 대기 중 (Hubble Server :4245 시뮬레이션)\n", addr)

	conn, err := listener.Accept()
	if err != nil {
		return
	}

	handleClient(conn, flowCount)
}

// ========================================
// 3. Client (Hubble CLI 역할)
// ========================================

// connectAndObserve는 'hubble observe --follow'를 시뮬레이션합니다.
//
// 실제 Hubble CLI에서는:
//   - gRPC 연결 수립 (TLS, port-forward 등 옵션)
//   - GetFlows RPC 호출 (필터 포함)
//   - 수신된 Flow를 Printer로 포맷팅하여 출력
func connectAndObserve(addr string) {
	// 서버가 준비될 때까지 잠시 대기
	time.Sleep(200 * time.Millisecond)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		fmt.Printf("[Client] 연결 실패: %v\n", err)
		return
	}
	defer conn.Close()

	fmt.Println("[Client] 서버에 연결됨 (hubble observe --follow 시뮬레이션)")
	fmt.Println("[Client] Flow 수신 시작...")
	fmt.Println()

	decoder := json.NewDecoder(conn)
	count := 0

	for {
		var flow Flow
		if err := decoder.Decode(&flow); err != nil {
			break
		}
		count++

		// Hubble CLI의 compact 출력 형식 시뮬레이션
		icon := "→"
		if flow.Verdict == "DROPPED" {
			icon = "✗"
		}

		fmt.Printf("  %s %s:%d %s %s:%d [%s] %s\n",
			flow.Timestamp.Format("15:04:05.000"),
			flow.Source, flow.Port,
			icon,
			flow.Destination, flow.Port,
			flow.Protocol,
			flow.Verdict,
		)
	}

	fmt.Printf("\n[Client] 총 %d개 Flow 수신 완료\n", count)
}

// ========================================
// 4. 실행
// ========================================

func main() {
	fmt.Println("=== PoC: gRPC Server Streaming 패턴 ===")
	fmt.Println()
	fmt.Println("이 PoC는 Hubble의 핵심 통신 패턴을 보여줍니다:")
	fmt.Println("  1. Server가 클라이언트 연결을 수락")
	fmt.Println("  2. eBPF 이벤트가 발생할 때마다 Flow를 스트리밍")
	fmt.Println("  3. Client가 실시간으로 Flow를 수신하여 출력")
	fmt.Println()
	fmt.Println("실제 Hubble에서는 gRPC + Protobuf를 사용하지만,")
	fmt.Println("이 PoC는 TCP + JSON으로 동일한 패턴을 시뮬레이션합니다.")
	fmt.Println()
	fmt.Println("-------------------------------------------")

	addr := "localhost:14245" // 실제 Hubble은 :4245
	flowCount := 10

	var wg sync.WaitGroup
	wg.Add(1)

	// Server 시작 (별도 goroutine)
	go startServer(addr, flowCount, &wg)

	// Client 연결 및 관찰
	connectAndObserve(addr)

	wg.Wait()

	fmt.Println()
	fmt.Println("-------------------------------------------")
	fmt.Println("핵심 포인트:")
	fmt.Println("  - Server Streaming: 서버가 연속적으로 데이터를 push")
	fmt.Println("  - 실제 Hubble: Observer.GetFlows() → stream GetFlowsResponse")
	fmt.Println("  - --follow 옵션: 무한 스트림 (Ctrl+C로 종료)")
	fmt.Println("  - --last N 옵션: N개 수신 후 자동 종료")
}
