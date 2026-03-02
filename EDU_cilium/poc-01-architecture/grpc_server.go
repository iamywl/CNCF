// gRPC 서버 스트리밍 — Hubble Relay가 Flow를 전달하는 패턴 재현
//
// Cilium 실제 동작:
//   hubble-relay가 각 노드의 hubble-observer로부터 Flow를 수집하고,
//   hubble CLI에게 gRPC 서버 스트리밍으로 전달한다.
//
// 실행: go run grpc_server.go
package main

import (
	"fmt"
	"log"
	"math/rand"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	// Cilium의 실제 protobuf 정의를 직접 사용
	flowpb "github.com/cilium/cilium/api/v1/flow"
	observerpb "github.com/cilium/cilium/api/v1/observer"
)

type server struct {
	observerpb.UnimplementedObserverServer
}

// GetFlows — Cilium의 실제 Hubble Observer와 동일한 RPC 시그니처
// 클라이언트가 한 번 호출하면, 서버가 Flow를 계속 스트리밍한다.
func (s *server) GetFlows(req *observerpb.GetFlowsRequest, stream observerpb.Observer_GetFlowsServer) error {
	log.Println("[relay] 클라이언트 연결됨, Flow 스트리밍 시작")

	verdicts := []flowpb.Verdict{
		flowpb.Verdict_FORWARDED,
		flowpb.Verdict_DROPPED,
		flowpb.Verdict_FORWARDED,
		flowpb.Verdict_FORWARDED,
		flowpb.Verdict_AUDIT,
	}

	dropReasons := []uint32{0, 181, 0, 0, 0} // 181 = POLICY_DENIED

	srcPods := []string{"frontend-7b4d8", "backend-3c9a1", "redis-5f2e7"}
	dstPods := []string{"backend-3c9a1", "db-8a1f3", "redis-5f2e7"}

	for i := 0; i < 20; i++ {
		idx := rand.Intn(len(verdicts))
		podIdx := rand.Intn(len(srcPods))

		flow := &flowpb.Flow{
			Time:    timestamppb.Now(),
			Verdict: verdicts[idx],
			Source: &flowpb.Endpoint{
				Namespace: "default",
				PodName:   srcPods[podIdx],
				Identity:  uint32(10000 + podIdx),
			},
			Destination: &flowpb.Endpoint{
				Namespace: "default",
				PodName:   dstPods[rand.Intn(len(dstPods))],
				Identity:  uint32(20000 + podIdx),
			},
			DropReason: dropReasons[idx],
			TrafficDirection: flowpb.TrafficDirection_EGRESS,
			L4: &flowpb.Layer4{
				Protocol: &flowpb.Layer4_TCP{
					TCP: &flowpb.TCP{
						SourcePort:      uint32(rand.Intn(60000) + 1024),
						DestinationPort: 80,
					},
				},
			},
		}

		resp := &observerpb.GetFlowsResponse{
			ResponseTypes: &observerpb.GetFlowsResponse_Flow{
				Flow: flow,
			},
		}

		if err := stream.Send(resp); err != nil {
			return err
		}

		// 실제 Hubble도 이벤트 발생 시마다 전송 — 여기서는 시뮬레이션
		time.Sleep(500 * time.Millisecond)
	}

	log.Println("[relay] 스트리밍 완료")
	return nil
}

func main() {
	lis, err := net.Listen("tcp", ":4245")
	if err != nil {
		log.Fatalf("listen 실패: %v", err)
	}

	s := grpc.NewServer()
	observerpb.RegisterObserverServer(s, &server{})

	fmt.Println("=== Hubble Relay (gRPC 서버) ===")
	fmt.Println("포트 :4245에서 대기 중...")
	fmt.Println("Cilium 실제 코드: hubble-relay/cmd/")
	fmt.Println()

	if err := s.Serve(lis); err != nil {
		log.Fatalf("serve 실패: %v", err)
	}
}
