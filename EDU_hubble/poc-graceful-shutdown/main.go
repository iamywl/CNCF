// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble 시그널 기반 Graceful Shutdown 패턴
//
// Hubble은 OS 시그널을 받으면 정리 작업 후 종료합니다:
//   - SIGINT (Ctrl+C), SIGTERM (kill) 처리
//   - context.WithCancel로 모든 goroutine에 취소 전파
//   - errgroup으로 동시 작업 관리 및 에러 전파
//   - 리소스 정리: TLS config, gRPC server, peer connections
//
// 실행: go run main.go
//   (3초 후 자동 종료 또는 Ctrl+C로 중단)

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// ========================================
// 1. 서버 컴포넌트 시뮬레이션
// ========================================

// GRPCServer는 gRPC 서버를 시뮬레이션합니다.
type GRPCServer struct {
	name string
}

func (s *GRPCServer) Start() {
	fmt.Printf("    [%s] 서버 시작됨\n", s.name)
}

func (s *GRPCServer) Stop() {
	fmt.Printf("    [%s] 서버 종료 중...\n", s.name)
	time.Sleep(200 * time.Millisecond) // 종료 작업 시뮬레이션
	fmt.Printf("    [%s] 서버 종료 완료\n", s.name)
}

// TLSConfigLoader는 TLS 인증서 로더를 시뮬레이션합니다.
type TLSConfigLoader struct {
	name string
}

func (t *TLSConfigLoader) Start() {
	fmt.Printf("    [%s] TLS 인증서 로더 시작\n", t.name)
}

func (t *TLSConfigLoader) Stop() {
	fmt.Printf("    [%s] TLS 인증서 감시 중지\n", t.name)
}

// PeerManager는 Peer 연결 관리자를 시뮬레이션합니다.
type PeerManager struct {
	peers []string
}

func (p *PeerManager) Start(ctx context.Context) {
	fmt.Printf("    [PeerManager] %d개 Peer 연결 관리 시작\n", len(p.peers))
}

func (p *PeerManager) Stop() {
	fmt.Printf("    [PeerManager] %d개 Peer 연결 종료 중...\n", len(p.peers))
	for _, peer := range p.peers {
		fmt.Printf("    [PeerManager] Peer %s 연결 해제\n", peer)
	}
	fmt.Printf("    [PeerManager] 모든 Peer 연결 해제 완료\n")
}

// ========================================
// 2. Background Worker (errgroup 패턴)
// ========================================

// ErrGroup은 golang.org/x/sync/errgroup을 시뮬레이션합니다.
// 실제 Hubble에서 errgroup으로 여러 goroutine을 관리합니다.
type ErrGroup struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
	err    error
}

func NewErrGroup(ctx context.Context) (*ErrGroup, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	return &ErrGroup{ctx: ctx, cancel: cancel}, ctx
}

func (g *ErrGroup) Go(f func(ctx context.Context) error) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := f(g.ctx); err != nil {
			g.once.Do(func() {
				g.err = err
				g.cancel()
			})
		}
	}()
}

func (g *ErrGroup) Wait() error {
	g.wg.Wait()
	g.cancel()
	return g.err
}

// ========================================
// 3. 스트림 워커 시뮬레이션
// ========================================

// flowCollector는 Flow 수집 워커를 시뮬레이션합니다.
func flowCollector(ctx context.Context, id int, flowCh chan<- string) error {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	count := 0
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("    [Collector-%d] 취소 시그널 수신, %d개 Flow 수집 후 종료\n", id, count)
			return nil
		case <-ticker.C:
			count++
			flow := fmt.Sprintf("flow-%d-%d", id, count)
			select {
			case flowCh <- flow:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// flowProcessor는 Flow 처리 워커를 시뮬레이션합니다.
func flowProcessor(ctx context.Context, flowCh <-chan string, processed *int) error {
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("    [Processor] 취소 시그널 수신, %d개 Flow 처리 후 종료\n", *processed)
			return nil
		case flow, ok := <-flowCh:
			if !ok {
				return nil
			}
			*processed++
			_ = flow
		}
	}
}

// ========================================
// 4. Graceful Shutdown 오케스트레이터
// ========================================

func main() {
	fmt.Println("=== PoC: Hubble Graceful Shutdown 패턴 ===")
	fmt.Println()
	fmt.Println("종료 순서:")
	fmt.Println("  1. OS 시그널 수신 (SIGINT/SIGTERM)")
	fmt.Println("  2. Context 취소 → 모든 goroutine에 전파")
	fmt.Println("  3. 워커 goroutine 종료 대기 (errgroup)")
	fmt.Println("  4. 리소스 정리 (역순): Peer → gRPC → TLS")
	fmt.Println()
	fmt.Println("3초 후 자동 종료됩니다 (또는 Ctrl+C로 중단)")
	fmt.Println()

	// 컴포넌트 초기화
	fmt.Println("── 1단계: 컴포넌트 초기화 ──")
	fmt.Println()

	grpcServer := &GRPCServer{name: "gRPC-Relay"}
	serverTLS := &TLSConfigLoader{name: "ServerTLS"}
	clientTLS := &TLSConfigLoader{name: "ClientTLS"}
	peerMgr := &PeerManager{peers: []string{"node-1:4244", "node-2:4244", "node-3:4244"}}

	// 시작 순서: TLS → gRPC → Peers
	serverTLS.Start()
	clientTLS.Start()
	grpcServer.Start()
	peerMgr.Start(context.Background())
	fmt.Println()

	// 시그널 핸들러 설정
	fmt.Println("── 2단계: 시그널 핸들러 및 워커 시작 ──")
	fmt.Println()

	// 시그널 채널 설정
	// 실제 Hubble: signal.Notify(sigs, unix.SIGINT, unix.SIGTERM)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	// errgroup으로 워커 관리
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	g, gctx := NewErrGroup(rootCtx)
	flowCh := make(chan string, 100)
	var processed int

	// Flow 수집 워커 3개 시작
	for i := 1; i <= 3; i++ {
		id := i
		g.Go(func(ctx context.Context) error {
			return flowCollector(ctx, id, flowCh)
		})
	}

	// Flow 처리 워커 시작
	g.Go(func(ctx context.Context) error {
		return flowProcessor(ctx, flowCh, &processed)
	})

	fmt.Println("  워커 시작: Collector x3, Processor x1")
	fmt.Println("  Flow 수집 중...")
	fmt.Println()

	// 시그널 또는 타임아웃 대기
	autoShutdown := time.NewTimer(3 * time.Second)
	select {
	case sig := <-sigs:
		fmt.Printf("── 3단계: 시그널 수신 (%s) → Graceful Shutdown 시작 ──\n", sig)
	case <-autoShutdown.C:
		fmt.Println("── 3단계: 타임아웃 → Graceful Shutdown 시작 ──")
	}
	fmt.Println()

	// Graceful Shutdown 실행
	shutdownStart := time.Now()

	// (1) Context 취소 → 모든 워커에 전파
	fmt.Println("  [Phase 1] Context 취소 전파")
	rootCancel()

	// (2) errgroup 완료 대기
	fmt.Println("  [Phase 2] 워커 종료 대기 (errgroup.Wait)")
	if err := g.Wait(); err != nil {
		fmt.Printf("  에러: %v\n", err)
	}
	close(flowCh)
	fmt.Println()

	// (3) 리소스 정리 (시작의 역순)
	fmt.Println("  [Phase 3] 리소스 정리 (역순)")

	_ = gctx // gctx 사용

	// 역순 정리: Peers → gRPC → TLS
	peerMgr.Stop()
	grpcServer.Stop()
	serverTLS.Stop()
	clientTLS.Stop()

	shutdownDuration := time.Since(shutdownStart)
	fmt.Println()
	fmt.Printf("  Graceful Shutdown 완료 (소요: %v)\n", shutdownDuration.Round(time.Millisecond))

	fmt.Println()
	fmt.Println("핵심 포인트:")
	fmt.Println("  - signal.Notify: OS 시그널을 Go 채널로 변환")
	fmt.Println("  - context.WithCancel: 모든 goroutine에 취소 전파")
	fmt.Println("  - errgroup: 여러 goroutine 관리 + 에러 전파")
	fmt.Println("  - 역순 정리: 의존성 순서의 역순으로 리소스 해제")
	fmt.Println("  - 실제 Hubble: TLS certloader, gRPC server, peer pool 순차 정리")
}
