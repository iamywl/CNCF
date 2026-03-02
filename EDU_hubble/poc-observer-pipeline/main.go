// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble Observer 전체 파이프라인
//
// Hubble Observer의 이벤트 처리 파이프라인을 시뮬레이션합니다:
//   이벤트 생성 → Hook(전처리) → 디코딩 → Hook(후처리) → 필터링 → Hook(전달전) → 전달
//
// 실행: go run main.go

package main

import (
	"context"
	"fmt"
	"math/rand"
	"time"
)

// ========================================
// 1. 데이터 타입
// ========================================

// MonitorEvent는 eBPF에서 올라오는 raw 이벤트입니다.
// 실제로는 바이트 배열이지만, 여기서는 구조체로 시뮬레이션합니다.
type MonitorEvent struct {
	CPU       int
	Timestamp time.Time
	RawData   string // 실제로는 []byte
	EventType string // "drop", "trace", "l7"
}

// Flow는 디코딩된 네트워크 플로우입니다.
type Flow struct {
	Timestamp   time.Time
	Source      string
	Destination string
	Verdict     string
	Protocol    string
	Port        int
	NodeName    string
}

func (f Flow) String() string {
	return fmt.Sprintf("%s %s → %s:%d [%s] %s",
		f.Timestamp.Format("15:04:05.000"),
		f.Source, f.Destination, f.Port, f.Protocol, f.Verdict)
}

// ========================================
// 2. Hook 시스템 (Observer의 확장 포인트)
// ========================================

// OnMonitorEventFunc는 raw 이벤트를 디코딩하기 전에 실행됩니다.
// 반환값: stop=true이면 이후 Hook 및 처리를 중단합니다.
type OnMonitorEventFunc func(ctx context.Context, ev *MonitorEvent) (stop bool, err error)

// OnDecodedFlowFunc는 Flow 디코딩 후 실행됩니다.
type OnDecodedFlowFunc func(ctx context.Context, flow *Flow) (stop bool, err error)

// OnFlowDeliveryFunc는 클라이언트에게 전달하기 직전에 실행됩니다.
type OnFlowDeliveryFunc func(ctx context.Context, flow *Flow) (stop bool, err error)

// ========================================
// 3. Observer (핵심 처리 엔진)
// ========================================

type Observer struct {
	// Hook 체인 (실제 Hubble의 observeroption.Options에 해당)
	onMonitorEvent []OnMonitorEventFunc
	onDecodedFlow  []OnDecodedFlowFunc
	onFlowDelivery []OnFlowDeliveryFunc

	// 통계
	received  int
	decoded   int
	filtered  int
	delivered int
	dropped   int
}

// processEvent는 하나의 MonitorEvent를 전체 파이프라인으로 처리합니다.
//
// 실제 Hubble Observer에서의 처리 순서:
//   1. MonitorEvent 수신 (perf ring buffer)
//   2. OnMonitorEvent hooks 실행 (전처리)
//   3. Parser.Decode() (raw → Flow 변환)
//   4. OnDecodedFlow hooks 실행 (enrichment, 메트릭)
//   5. Filter 적용 (whitelist/blacklist)
//   6. OnFlowDelivery hooks 실행 (전달 전 처리)
//   7. Ring Buffer 저장 + 클라이언트 스트리밍
func (o *Observer) processEvent(ctx context.Context, ev MonitorEvent, filters []func(Flow) bool) *Flow {
	o.received++
	step := fmt.Sprintf("  [%d]", o.received)

	// ── 1단계: OnMonitorEvent hooks ──
	fmt.Printf("%s 1. OnMonitorEvent hooks (raw 이벤트: %s, type=%s)\n", step, ev.RawData, ev.EventType)
	for _, hook := range o.onMonitorEvent {
		stop, err := hook(ctx, &ev)
		if err != nil {
			fmt.Printf("%s    ✗ Hook 에러: %v → 이벤트 폐기\n", step, err)
			return nil
		}
		if stop {
			fmt.Printf("%s    ■ Hook이 처리 중단 요청\n", step)
			return nil
		}
	}

	// ── 2단계: Decode (raw → Flow) ──
	flow := decode(ev)
	if flow == nil {
		fmt.Printf("%s 2. Decode 실패 → 이벤트 폐기\n", step)
		return nil
	}
	o.decoded++
	fmt.Printf("%s 2. Decoded: %s\n", step, flow)

	// ── 3단계: OnDecodedFlow hooks ──
	for _, hook := range o.onDecodedFlow {
		stop, err := hook(ctx, flow)
		if err != nil || stop {
			fmt.Printf("%s 3. OnDecodedFlow: 처리 중단\n", step)
			return nil
		}
	}
	fmt.Printf("%s 3. OnDecodedFlow hooks 통과\n", step)

	// ── 4단계: Filter 적용 ──
	for _, filter := range filters {
		if !filter(*flow) {
			o.filtered++
			fmt.Printf("%s 4. Filter: 불일치 → 스킵\n", step)
			return nil
		}
	}
	fmt.Printf("%s 4. Filter: 통과\n", step)

	// ── 5단계: OnFlowDelivery hooks ──
	for _, hook := range o.onFlowDelivery {
		stop, err := hook(ctx, flow)
		if err != nil || stop {
			fmt.Printf("%s 5. OnFlowDelivery: 전달 중단\n", step)
			return nil
		}
	}

	o.delivered++
	fmt.Printf("%s 5. ✓ 클라이언트에 전달!\n", step)
	return flow
}

// ========================================
// 4. Parser (Decoder)
// ========================================

// decode는 raw MonitorEvent를 Flow로 변환합니다.
//
// 실제 Hubble Parser에서는:
//   - L3/L4 파서: 이더넷/IP/TCP/UDP 헤더 파싱
//   - L7 파서: DNS/HTTP 페이로드 파싱
//   - Getter 인터페이스로 K8s 메타데이터 enrichment
func decode(ev MonitorEvent) *Flow {
	pods := []string{"frontend", "backend", "database", "cache"}
	protocols := []string{"TCP", "UDP", "HTTP", "DNS"}
	ports := []int{80, 443, 8080, 53, 3306}

	verdict := "FORWARDED"
	if ev.EventType == "drop" {
		verdict = "DROPPED"
	}

	return &Flow{
		Timestamp:   ev.Timestamp,
		Source:      fmt.Sprintf("default/%s", pods[rand.Intn(len(pods))]),
		Destination: fmt.Sprintf("default/%s", pods[rand.Intn(len(pods))]),
		Verdict:     verdict,
		Protocol:    protocols[rand.Intn(len(protocols))],
		Port:        ports[rand.Intn(len(ports))],
		NodeName:    "worker-node-1",
	}
}

// ========================================
// 5. 실행
// ========================================

func main() {
	fmt.Println("=== PoC: Hubble Observer 파이프라인 ===")
	fmt.Println()
	fmt.Println("파이프라인 순서:")
	fmt.Println("  MonitorEvent → [OnMonitorEvent] → Decode → [OnDecodedFlow] → Filter → [OnFlowDelivery] → 전달")
	fmt.Println()
	fmt.Println("-------------------------------------------")
	fmt.Println()

	ctx := context.Background()

	observer := &Observer{
		// Hook 1: 이벤트 유효성 검증 (OnMonitorEvent)
		onMonitorEvent: []OnMonitorEventFunc{
			func(ctx context.Context, ev *MonitorEvent) (bool, error) {
				fmt.Printf("       → [검증 Hook] CPU=%d, type=%s\n", ev.CPU, ev.EventType)
				if ev.RawData == "" {
					return true, fmt.Errorf("empty event")
				}
				return false, nil
			},
		},

		// Hook 2: 메트릭 수집 (OnDecodedFlow)
		onDecodedFlow: []OnDecodedFlowFunc{
			func(ctx context.Context, flow *Flow) (bool, error) {
				// 실제 Hubble: Prometheus counter/histogram 업데이트
				fmt.Printf("       → [메트릭 Hook] %s 플로우 카운트 +1\n", flow.Verdict)
				return false, nil
			},
		},

		// Hook 3: 감사 로깅 (OnFlowDelivery)
		onFlowDelivery: []OnFlowDeliveryFunc{
			func(ctx context.Context, flow *Flow) (bool, error) {
				if flow.Verdict == "DROPPED" {
					fmt.Printf("       → [감사 Hook] ⚠ DROP 이벤트 로깅: %s → %s\n",
						flow.Source, flow.Destination)
				}
				return false, nil
			},
		},
	}

	// 필터: verdict가 DROPPED인 것만 통과 (hubble observe --verdict DROPPED)
	filters := []func(Flow) bool{
		func(f Flow) bool {
			return f.Verdict == "DROPPED"
		},
	}

	fmt.Println("필터 설정: --verdict DROPPED (차단된 트래픽만 관찰)")
	fmt.Println()

	// 이벤트 생성 및 처리
	events := []MonitorEvent{
		{CPU: 0, Timestamp: time.Now(), RawData: "pkt_001", EventType: "trace"},
		{CPU: 1, Timestamp: time.Now(), RawData: "pkt_002", EventType: "drop"},
		{CPU: 0, Timestamp: time.Now(), RawData: "pkt_003", EventType: "trace"},
		{CPU: 2, Timestamp: time.Now(), RawData: "", EventType: "trace"}, // 빈 이벤트 → Hook에서 거부
		{CPU: 1, Timestamp: time.Now(), RawData: "pkt_005", EventType: "drop"},
		{CPU: 0, Timestamp: time.Now(), RawData: "pkt_006", EventType: "l7"},
	}

	var delivered []*Flow
	for _, ev := range events {
		fmt.Println()
		if f := observer.processEvent(ctx, ev, filters); f != nil {
			delivered = append(delivered, f)
		}
	}

	fmt.Println()
	fmt.Println("-------------------------------------------")
	fmt.Printf("\n[통계]\n")
	fmt.Printf("  수신:   %d 이벤트\n", observer.received)
	fmt.Printf("  디코딩: %d 이벤트\n", observer.decoded)
	fmt.Printf("  필터:   %d 이벤트 필터링됨\n", observer.filtered)
	fmt.Printf("  전달:   %d 이벤트 (클라이언트에 전달)\n", observer.delivered)
	fmt.Println()

	if len(delivered) > 0 {
		fmt.Println("[전달된 Flow 목록]")
		for _, f := range delivered {
			fmt.Printf("  ✓ %s\n", f)
		}
	}

	fmt.Println()
	fmt.Println("핵심 포인트:")
	fmt.Println("  - 5단계 파이프라인: 각 단계에서 이벤트를 거부/변형 가능")
	fmt.Println("  - Hook 체인: stop=true 반환 시 이후 처리 중단")
	fmt.Println("  - 필터: whitelist/blacklist로 관심 있는 Flow만 통과")
	fmt.Println("  - 실제 Hubble: 초당 수천~수만 이벤트를 이 파이프라인으로 처리")
}
