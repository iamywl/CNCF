// poc-15-flow-control: HTTP/2 윈도우 기반 흐름 제어 시뮬레이션
//
// grpc-go의 내부 전송 계층 흐름 제어를 표준 라이브러리만으로 재현한다.
// - 연결 레벨 + 스트림 레벨 이중 흐름 제어
// - 초기 윈도우 크기 (65535 바이트)
// - DATA 전송 시 윈도우 감소
// - WINDOW_UPDATE 수신 시 윈도우 증가
// - 윈도우 고갈 시 전송 대기
// - BDP (Bandwidth Delay Product) 추정 시뮬레이션
//
// 실제 grpc-go 소스: internal/transport/flowcontrol.go
package main

import (
	"fmt"
	"sync"
	"time"
)

// ========== 상수 ==========
// grpc-go: internal/transport/defaults.go
const (
	initialWindowSize     = 65535 // HTTP/2 기본 윈도우 크기 (RFC 7540)
	defaultFrameSize      = 16384 // HTTP/2 기본 프레임 크기
	maxWindowSize         = (1 << 31) - 1 // 최대 윈도우 크기 (2^31 - 1)
	windowUpdateThreshold = initialWindowSize / 4 // 윈도우 업데이트 임계값
)

// ========== 인바운드 흐름 제어 (trInFlow) ==========
// grpc-go: internal/transport/flowcontrol.go 81행
// 수신 측에서 DATA를 받을 때마다 윈도우가 줄어들고,
// 임계값 이하로 내려가면 WINDOW_UPDATE를 보내 상대방에게 윈도우를 돌려준다.
type InboundFlow struct {
	mu                  sync.Mutex
	limit               uint32 // 총 윈도우 크기
	unacked             uint32 // 아직 ACK하지 않은 바이트 (WINDOW_UPDATE로 반환할 양)
	effectiveWindowSize uint32 // 실효 윈도우 = limit - unacked
	label               string
}

func NewInboundFlow(label string, limit uint32) *InboundFlow {
	return &InboundFlow{
		limit:               limit,
		effectiveWindowSize: limit,
		label:               label,
	}
}

// OnData는 DATA 프레임을 수신했을 때 호출된다.
func (f *InboundFlow) OnData(size uint32) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.unacked += size
	f.effectiveWindowSize = f.limit - f.unacked

	// 윈도우 업데이트 필요 여부 판단
	// grpc-go: 사용한 양이 임계값(1/4)을 넘으면 WINDOW_UPDATE 전송
	var windowUpdate uint32
	if f.unacked >= windowUpdateThreshold {
		windowUpdate = f.unacked
		f.unacked = 0
		f.effectiveWindowSize = f.limit
	}

	return windowUpdate // 0이면 WINDOW_UPDATE 불필요
}

func (f *InboundFlow) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprintf("%s: effective=%d, unacked=%d, limit=%d",
		f.label, f.effectiveWindowSize, f.unacked, f.limit)
}

// ========== 아웃바운드 흐름 제어 (writeQuota) ==========
// grpc-go: internal/transport/flowcontrol.go 30행
// 전송 측에서 DATA를 보내기 전에 윈도우를 확인하고,
// 윈도우가 부족하면 WINDOW_UPDATE가 올 때까지 대기한다.
type OutboundFlow struct {
	mu        sync.Mutex
	quota     int32      // 남은 전송 가능 바이트
	waitCh    chan struct{} // quota가 양수가 되면 신호
	label     string
}

func NewOutboundFlow(label string, initial int32) *OutboundFlow {
	return &OutboundFlow{
		quota:  initial,
		waitCh: make(chan struct{}, 1),
		label:  label,
	}
}

// Acquire는 전송할 바이트만큼 윈도우를 소비한다.
// 윈도우가 부족하면 대기한다.
func (f *OutboundFlow) Acquire(size int32) bool {
	for {
		f.mu.Lock()
		if f.quota >= size {
			f.quota -= size
			remaining := f.quota
			f.mu.Unlock()
			fmt.Printf("    [%s] 윈도우 소비: -%d, 잔여=%d\n", f.label, size, remaining)
			return true
		}

		// 가용 윈도우가 부족: WINDOW_UPDATE 대기
		available := f.quota
		f.mu.Unlock()
		fmt.Printf("    [%s] 윈도우 부족! 필요=%d, 가용=%d — 대기 중...\n", f.label, size, available)

		// WINDOW_UPDATE 대기 (타임아웃 포함)
		select {
		case <-f.waitCh:
			fmt.Printf("    [%s] WINDOW_UPDATE 수신 — 재시도\n", f.label)
			continue
		case <-time.After(500 * time.Millisecond):
			fmt.Printf("    [%s] 대기 타임아웃!\n", f.label)
			return false
		}
	}
}

// Replenish는 WINDOW_UPDATE를 수신했을 때 윈도우를 보충한다.
func (f *OutboundFlow) Replenish(size int32) {
	f.mu.Lock()
	f.quota += size
	remaining := f.quota
	f.mu.Unlock()
	fmt.Printf("    [%s] WINDOW_UPDATE +%d, 잔여=%d\n", f.label, size, remaining)

	// 대기 중인 전송을 깨운다
	select {
	case f.waitCh <- struct{}{}:
	default:
	}
}

func (f *OutboundFlow) GetQuota() int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.quota
}

// ========== 이중 흐름 제어 ==========
// HTTP/2는 연결 레벨과 스트림 레벨 두 개의 윈도우를 동시에 적용한다.
// 전송하려면 양쪽 윈도우 모두 충분해야 한다.
type DualFlowControl struct {
	connFlow   *OutboundFlow // 연결 레벨
	streamFlow *OutboundFlow // 스트림 레벨
}

func NewDualFlowControl(connWindow, streamWindow int32) *DualFlowControl {
	return &DualFlowControl{
		connFlow:   NewOutboundFlow("conn-level", connWindow),
		streamFlow: NewOutboundFlow("stream-level", streamWindow),
	}
}

// Send는 이중 윈도우를 모두 소비하여 데이터를 전송한다.
func (d *DualFlowControl) Send(size int32) bool {
	// 스트림 레벨 먼저
	if !d.streamFlow.Acquire(size) {
		return false
	}
	// 연결 레벨
	if !d.connFlow.Acquire(size) {
		// 스트림 윈도우는 소비했지만 연결 윈도우 부족 → 롤백 필요
		d.streamFlow.Replenish(size)
		return false
	}
	return true
}

// ========== BDP 추정 ==========
// grpc-go: internal/transport/bdp_estimator.go
// BDP(Bandwidth Delay Product)를 추정하여 윈도우 크기를 동적으로 조절한다.
// BDP = Bandwidth × RTT
type BDPEstimator struct {
	mu          sync.Mutex
	samples     []bdpSample
	currentBDP  int
	sentPingAt  time.Time
	sampleCount int
}

type bdpSample struct {
	bw  float64 // bytes/sec
	rtt time.Duration
	bdp int
}

func NewBDPEstimator() *BDPEstimator {
	return &BDPEstimator{currentBDP: initialWindowSize}
}

// AddSample은 PING/PONG RTT와 전송량으로 BDP를 추정한다.
func (e *BDPEstimator) AddSample(bytesTransferred int, rtt time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if rtt <= 0 {
		return
	}

	bw := float64(bytesTransferred) / rtt.Seconds()
	bdp := int(bw * rtt.Seconds())

	sample := bdpSample{bw: bw, rtt: rtt, bdp: bdp}
	e.samples = append(e.samples, sample)
	e.sampleCount++

	// BDP가 현재 윈도우의 2배 이상이면 윈도우 확장 권장
	if bdp > e.currentBDP*2 {
		e.currentBDP = bdp
	}
}

func (e *BDPEstimator) GetRecommendedWindow() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	// 최소 초기 윈도우, 최대 16MB
	if e.currentBDP < initialWindowSize {
		return initialWindowSize
	}
	if e.currentBDP > 16*1024*1024 {
		return 16 * 1024 * 1024
	}
	return e.currentBDP
}

func main() {
	fmt.Println("========================================")
	fmt.Println("HTTP/2 흐름 제어 시뮬레이션")
	fmt.Println("========================================")

	// 1. 기본 윈도우 크기
	fmt.Println("\n[1] HTTP/2 흐름 제어 기본 설정")
	fmt.Println("────────────────────────────────")
	fmt.Printf("  초기 윈도우 크기:     %d bytes (%.1f KB)\n", initialWindowSize, float64(initialWindowSize)/1024)
	fmt.Printf("  기본 프레임 크기:     %d bytes (%.1f KB)\n", defaultFrameSize, float64(defaultFrameSize)/1024)
	fmt.Printf("  최대 윈도우 크기:     %d bytes (%.1f GB)\n", maxWindowSize, float64(maxWindowSize)/1024/1024/1024)
	fmt.Printf("  WINDOW_UPDATE 임계값: %d bytes\n", windowUpdateThreshold)

	// 2. 인바운드 흐름 제어
	fmt.Println("\n[2] 인바운드 흐름 제어 (수신 측)")
	fmt.Println("─────────────────────────────────")
	fmt.Println("  DATA 수신 → unacked 증가 → 임계값 도달 시 WINDOW_UPDATE 전송")
	fmt.Println()

	inflow := NewInboundFlow("recv", initialWindowSize)
	fmt.Printf("  초기 상태: %s\n", inflow)

	// 여러 번 DATA 수신
	dataChunks := []uint32{10000, 10000, 10000, 10000}
	for i, chunk := range dataChunks {
		wu := inflow.OnData(chunk)
		if wu > 0 {
			fmt.Printf("  DATA #%d (%d bytes): → WINDOW_UPDATE %d bytes 전송\n", i+1, chunk, wu)
		} else {
			fmt.Printf("  DATA #%d (%d bytes): 아직 임계값 미달\n", i+1, chunk)
		}
		fmt.Printf("    상태: %s\n", inflow)
	}

	// 3. 아웃바운드 흐름 제어
	fmt.Println("\n[3] 아웃바운드 흐름 제어 (송신 측)")
	fmt.Println("───────────────────────────────────")
	fmt.Println("  DATA 전송 전 윈도우 확인 → 부족하면 대기 → WINDOW_UPDATE 수신 후 재개")
	fmt.Println()

	outflow := NewOutboundFlow("send", 30000) // 작은 윈도우로 시작
	fmt.Printf("  초기 윈도우: %d\n\n", outflow.GetQuota())

	// 데이터 전송
	fmt.Println("  전송 #1: 16384 bytes")
	outflow.Acquire(16384)

	fmt.Println("\n  전송 #2: 16384 bytes")
	// 윈도우 부족 발생 → 비동기로 WINDOW_UPDATE
	go func() {
		time.Sleep(50 * time.Millisecond)
		outflow.Replenish(20000)
	}()
	outflow.Acquire(16384)

	// 4. 이중 흐름 제어
	fmt.Println("\n[4] 이중 흐름 제어 (연결 + 스트림)")
	fmt.Println("────────────────────────────────────")
	fmt.Println("  HTTP/2는 연결 레벨과 스트림 레벨 윈도우를 모두 적용한다.")
	fmt.Println("  전송하려면 양쪽 모두 충분한 윈도우가 필요하다.")
	fmt.Println()

	dual := NewDualFlowControl(40000, 30000)
	fmt.Printf("  연결 윈도우: %d, 스트림 윈도우: %d\n\n", 40000, 30000)

	fmt.Println("  전송 #1: 16384 bytes (양쪽 모두 충분)")
	ok := dual.Send(16384)
	fmt.Printf("  결과: %v\n\n", ok)

	fmt.Println("  전송 #2: 16384 bytes (스트림 윈도우 부족 발생 가능)")
	go func() {
		time.Sleep(50 * time.Millisecond)
		dual.streamFlow.Replenish(20000)
	}()
	ok = dual.Send(16384)
	fmt.Printf("  결과: %v\n", ok)

	// 5. 윈도우 고갈 시나리오
	fmt.Println("\n[5] 윈도우 고갈 시나리오")
	fmt.Println("─────────────────────────")
	fmt.Println("  대량 데이터 전송 시 윈도우가 고갈되는 과정:")
	fmt.Println()

	smallWindow := NewOutboundFlow("small", int32(initialWindowSize))
	totalSent := 0
	frameCount := 0
	for {
		remaining := smallWindow.GetQuota()
		if remaining < int32(defaultFrameSize) {
			fmt.Printf("\n  윈도우 고갈! 잔여=%d bytes, 총 전송=%d bytes (%d 프레임)\n",
				remaining, totalSent, frameCount)
			break
		}
		smallWindow.Acquire(int32(defaultFrameSize))
		totalSent += defaultFrameSize
		frameCount++
	}

	// 6. BDP 추정
	fmt.Println("\n[6] BDP 추정 시뮬레이션")
	fmt.Println("────────────────────────")
	fmt.Println("  BDP = Bandwidth x RTT")
	fmt.Println("  PING/PONG RTT 측정으로 네트워크 상태를 추정하고 윈도우를 조절한다.")
	fmt.Println()

	bdp := NewBDPEstimator()

	scenarios := []struct {
		name    string
		bytes   int
		rtt     time.Duration
	}{
		{"로컬 네트워크", 65535, 1 * time.Millisecond},
		{"같은 리전", 65535, 5 * time.Millisecond},
		{"대륙 간", 65535, 100 * time.Millisecond},
		{"대용량 전송", 1024 * 1024, 50 * time.Millisecond},
	}

	for _, s := range scenarios {
		bdp.AddSample(s.bytes, s.rtt)
		bw := float64(s.bytes) / s.rtt.Seconds()
		calculated := int(bw * s.rtt.Seconds())
		recommended := bdp.GetRecommendedWindow()
		fmt.Printf("  %-14s: BW=%.1f MB/s, RTT=%v, BDP=%d bytes → 권장 윈도우=%d bytes\n",
			s.name, bw/1024/1024, s.rtt, calculated, recommended)
	}

	// 7. 흐름 제어 전체 다이어그램
	fmt.Println("\n[7] 흐름 제어 전체 흐름")
	fmt.Println("────────────────────────")
	fmt.Println("  송신 측                            수신 측")
	fmt.Println("    │                                  │")
	fmt.Println("    │  DATA (16KB)                     │")
	fmt.Println("    │─────────────────────────────────→│")
	fmt.Println("    │  conn_window -= 16KB             │  conn_window.unacked += 16KB")
	fmt.Println("    │  stream_window -= 16KB           │  stream_window.unacked += 16KB")
	fmt.Println("    │                                  │")
	fmt.Println("    │  DATA (16KB)                     │")
	fmt.Println("    │─────────────────────────────────→│")
	fmt.Println("    │                                  │  unacked >= threshold?")
	fmt.Println("    │                                  │  → WINDOW_UPDATE 전송")
	fmt.Println("    │  WINDOW_UPDATE (32KB)            │")
	fmt.Println("    │←─────────────────────────────────│")
	fmt.Println("    │  conn_window += 32KB             │")
	fmt.Println("    │                                  │")
	fmt.Println("    │  ... (반복) ...                  │")
	fmt.Println("    │                                  │")
	fmt.Println("    │  (윈도우 고갈)                    │")
	fmt.Println("    │  전송 대기 ──────→ WINDOW_UPDATE │")
	fmt.Println("    │←─────────────────────────────────│")
	fmt.Println("    │  전송 재개                        │")

	fmt.Println("\n========================================")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("========================================")
}
