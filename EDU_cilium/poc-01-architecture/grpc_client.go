// gRPC 클라이언트 — hubble CLI가 Flow를 수신하는 패턴 재현
//
// Cilium 실제 동작:
//   `hubble observe` 명령을 실행하면 hubble-relay에 GetFlows RPC를 호출하고,
//   서버가 보내는 Flow 스트림을 받아 화면에 출력한다.
//
// 실행: go run grpc_client.go
package main

import (
	"context"
	"fmt"
	"io"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	flowpb "github.com/cilium/cilium/api/v1/flow"
	observerpb "github.com/cilium/cilium/api/v1/observer"
)

func verdictSymbol(v flowpb.Verdict) string {
	switch v {
	case flowpb.Verdict_FORWARDED:
		return ">>  FORWARDED"
	case flowpb.Verdict_DROPPED:
		return "xx  DROPPED  "
	case flowpb.Verdict_AUDIT:
		return "~~  AUDIT    "
	default:
		return "??  UNKNOWN  "
	}
}

func main() {
	fmt.Println("=== Hubble CLI (gRPC 클라이언트) ===")
	fmt.Println("hubble-relay(:4245)에 연결 중...")
	fmt.Println("Cilium 실제 코드: hubble/cmd/observe/observe.go")
	fmt.Println()

	conn, err := grpc.NewClient(
		"localhost:4245",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatalf("연결 실패: %v", err)
	}
	defer conn.Close()

	client := observerpb.NewObserverClient(conn)

	// GetFlows — 서버 스트리밍 RPC 호출
	// 한 번 호출하면 서버가 계속 Flow를 보내준다
	stream, err := client.GetFlows(context.Background(), &observerpb.GetFlowsRequest{
		// Number: 20, // 최대 수신 개수 (0이면 무제한)
	})
	if err != nil {
		log.Fatalf("GetFlows 호출 실패: %v", err)
	}

	fmt.Println("Flow 스트림 수신 시작:")
	fmt.Println("─────────────────────────────────────────────────────────────")

	count := 0
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatalf("수신 에러: %v", err)
		}

		flow := resp.GetFlow()
		if flow == nil {
			continue
		}

		count++
		ts := flow.GetTime().AsTime().Format("15:04:05.000")
		verdict := verdictSymbol(flow.GetVerdict())
		src := fmt.Sprintf("%s/%s (ID:%d)",
			flow.GetSource().GetNamespace(),
			flow.GetSource().GetPodName(),
			flow.GetSource().GetIdentity(),
		)
		dst := fmt.Sprintf("%s/%s (ID:%d)",
			flow.GetDestination().GetNamespace(),
			flow.GetDestination().GetPodName(),
			flow.GetDestination().GetIdentity(),
		)

		port := ""
		if tcp := flow.GetL4().GetTCP(); tcp != nil {
			port = fmt.Sprintf("TCP %d->%d", tcp.GetSourcePort(), tcp.GetDestinationPort())
		}

		fmt.Printf("[%s] %s  %s -> %s  %s\n", ts, verdict, src, dst, port)

		if flow.GetVerdict() == flowpb.Verdict_DROPPED {
			fmt.Printf("           ^^ DROP 사유 코드: %d (POLICY_DENIED)\n", flow.GetDropReason())
		}
	}

	fmt.Println("─────────────────────────────────────────────────────────────")
	fmt.Printf("총 %d개 Flow 수신 완료\n", count)
}
