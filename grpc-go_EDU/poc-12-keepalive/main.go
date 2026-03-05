// poc-12-keepalive: gRPC Keepalive 핑/퐁 및 유휴 관리 시뮬레이션
//
// grpc-go의 keepalive 패키지 핵심 개념을 표준 라이브러리만으로 재현한다.
// - TCP 연결에서 주기적 PING/PONG
// - 클라이언트 keepalive: Time, Timeout, PermitWithoutStream
// - 서버 keepalive: MaxConnectionIdle, MaxConnectionAge
// - EnforcementPolicy: 핑 과다 시 GOAWAY
// - 타임아웃 시 연결 종료
//
// 실제 grpc-go 소스: keepalive/keepalive.go, internal/transport/keepalive_test.go
package main

import (
	"fmt"
	"sync"
	"time"
)

// ========== Keepalive 파라미터 ==========
// grpc-go: keepalive/keepalive.go 33행

// ClientParameters는 클라이언트 측 keepalive 설정이다.
type ClientParameters struct {
	Time                time.Duration // 활동 없을 때 PING 전송 간격 (기본 무한)
	Timeout             time.Duration // PING 후 PONG 대기 시간 (기본 20초)
	PermitWithoutStream bool          // 활성 스트림 없어도 PING 허용
}

// ServerParameters는 서버 측 keepalive 설정이다.
// grpc-go: keepalive/keepalive.go 64행
type ServerParameters struct {
	MaxConnectionIdle     time.Duration // 유휴 연결 최대 지속 시간
	MaxConnectionAge      time.Duration // 연결 최대 수명
	MaxConnectionAgeGrace time.Duration // MaxConnectionAge 후 유예 시간
	Time                  time.Duration // 서버→클라이언트 PING 간격
	Timeout               time.Duration // PONG 대기 시간
}

// EnforcementPolicy는 서버의 핑 제한 정책이다.
// grpc-go: keepalive/keepalive.go EnforcementPolicy
type EnforcementPolicy struct {
	MinTime             time.Duration // 클라이언트 PING 최소 간격 (기본 5분)
	PermitWithoutStream bool          // 활성 스트림 없이 PING 허용 여부
}

// ========== 프레임 타입 ==========
type FrameType int

const (
	PingFrame   FrameType = iota // PING 프레임
	PongFrame                    // PING ACK (PONG)
	GoAwayFrame                  // 연결 종료 요청
	DataFrame                    // 데이터 프레임 (활동 표시)
)

func (f FrameType) String() string {
	switch f {
	case PingFrame:
		return "PING"
	case PongFrame:
		return "PONG"
	case GoAwayFrame:
		return "GOAWAY"
	case DataFrame:
		return "DATA"
	default:
		return "UNKNOWN"
	}
}

type Frame struct {
	Type    FrameType
	Payload string
}

// ========== Connection 시뮬레이션 ==========
type Connection struct {
	mu             sync.Mutex
	id             string
	active         bool           // 연결 활성 상태
	hasStream      bool           // 활성 스트림 유무
	lastActivity   time.Time      // 마지막 활동 시각
	createdAt      time.Time      // 연결 생성 시각
	pingCount      int            // 수신한 PING 수
	lastPingTime   time.Time      // 마지막 PING 수신 시각
	frames         chan Frame     // 프레임 채널
	done           chan struct{}  // 종료 신호
	log            []string       // 이벤트 로그
}

func NewConnection(id string) *Connection {
	now := time.Now()
	return &Connection{
		id:           id,
		active:       true,
		lastActivity: now,
		createdAt:    now,
		frames:       make(chan Frame, 100),
		done:         make(chan struct{}),
	}
}

func (c *Connection) logEvent(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elapsed := time.Since(c.createdAt).Truncate(time.Millisecond)
	entry := fmt.Sprintf("  [%s +%v] %s", c.id, elapsed, msg)
	c.log = append(c.log, entry)
}

func (c *Connection) sendFrame(f Frame) {
	select {
	case c.frames <- f:
	case <-c.done:
	}
}

func (c *Connection) close(reason string) {
	c.mu.Lock()
	if !c.active {
		c.mu.Unlock()
		return
	}
	c.active = false
	c.mu.Unlock()
	c.logEvent(fmt.Sprintf("연결 종료: %s", reason))
	close(c.done)
}

func (c *Connection) isActive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

func (c *Connection) recordActivity() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastActivity = time.Now()
}

func (c *Connection) printLog() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.log {
		fmt.Println(entry)
	}
}

// ========== 클라이언트 Keepalive ==========
// grpc-go: internal/transport/http2_client.go keepalive() 메서드
// 주기적으로 PING을 보내고, 응답을 기다린다.
func runClientKeepalive(conn *Connection, params ClientParameters) {
	conn.logEvent(fmt.Sprintf("클라이언트 keepalive 시작 (Time=%v, Timeout=%v)", params.Time, params.Timeout))

	timer := time.NewTimer(params.Time)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			if !conn.isActive() {
				return
			}

			conn.mu.Lock()
			idle := time.Since(conn.lastActivity)
			hasStream := conn.hasStream
			conn.mu.Unlock()

			// 활성 스트림 없고 PermitWithoutStream이 false이면 PING 스킵
			if !hasStream && !params.PermitWithoutStream {
				conn.logEvent("활성 스트림 없음 — PING 스킵")
				timer.Reset(params.Time)
				continue
			}

			// 활동이 있었으면 PING 불필요
			if idle < params.Time {
				conn.logEvent(fmt.Sprintf("최근 활동 있음 (%v 전) — PING 스킵", idle.Truncate(time.Millisecond)))
				timer.Reset(params.Time - idle)
				continue
			}

			// PING 전송
			conn.logEvent("→ PING 전송")
			conn.sendFrame(Frame{Type: PingFrame, Payload: "client-ping"})

			// PONG 대기
			pongTimer := time.NewTimer(params.Timeout)
			select {
			case f := <-conn.frames:
				pongTimer.Stop()
				if f.Type == PongFrame {
					conn.logEvent("← PONG 수신 — 연결 정상")
					conn.recordActivity()
				} else if f.Type == GoAwayFrame {
					conn.logEvent(fmt.Sprintf("← GOAWAY 수신: %s", f.Payload))
					conn.close("서버 GOAWAY")
					return
				}
			case <-pongTimer.C:
				conn.logEvent(fmt.Sprintf("PONG 타임아웃 (%v) — 연결 종료", params.Timeout))
				conn.close("keepalive timeout")
				return
			case <-conn.done:
				pongTimer.Stop()
				return
			}
			timer.Reset(params.Time)

		case <-conn.done:
			return
		}
	}
}

// ========== 서버 Keepalive ==========
// grpc-go: internal/transport/http2_server.go keepalive() 메서드
func runServerKeepalive(conn *Connection, params ServerParameters, policy EnforcementPolicy) {
	conn.logEvent(fmt.Sprintf("서버 keepalive 시작 (MaxIdle=%v, MaxAge=%v)", params.MaxConnectionIdle, params.MaxConnectionAge))

	idleTimer := time.NewTimer(params.MaxConnectionIdle)
	ageTimer := time.NewTimer(params.MaxConnectionAge)
	pingTimer := time.NewTimer(params.Time)

	defer idleTimer.Stop()
	defer ageTimer.Stop()
	defer pingTimer.Stop()

	for {
		select {
		// 유휴 시간 초과
		case <-idleTimer.C:
			if !conn.isActive() {
				return
			}
			conn.mu.Lock()
			idle := time.Since(conn.lastActivity)
			hasStream := conn.hasStream
			conn.mu.Unlock()

			if !hasStream && idle >= params.MaxConnectionIdle {
				conn.logEvent(fmt.Sprintf("유휴 시간 초과 (%v) — GOAWAY 전송", idle.Truncate(time.Millisecond)))
				conn.sendFrame(Frame{Type: GoAwayFrame, Payload: "max_idle_exceeded"})
				conn.close("MaxConnectionIdle 초과")
				return
			}
			idleTimer.Reset(params.MaxConnectionIdle)

		// 연결 수명 초과
		case <-ageTimer.C:
			if !conn.isActive() {
				return
			}
			age := time.Since(conn.createdAt)
			conn.logEvent(fmt.Sprintf("연결 수명 초과 (%v) — GOAWAY 전송", age.Truncate(time.Millisecond)))
			conn.sendFrame(Frame{Type: GoAwayFrame, Payload: "max_age_exceeded"})

			// Grace period: 진행 중인 RPC가 완료될 시간을 준다
			if params.MaxConnectionAgeGrace > 0 {
				conn.logEvent(fmt.Sprintf("유예 시간 %v 대기", params.MaxConnectionAgeGrace))
				time.Sleep(params.MaxConnectionAgeGrace)
			}
			conn.close("MaxConnectionAge 초과")
			return

		// 서버 → 클라이언트 PING
		case <-pingTimer.C:
			if !conn.isActive() {
				return
			}
			conn.logEvent("→ PING 전송 (서버→클라이언트)")
			conn.sendFrame(Frame{Type: PingFrame, Payload: "server-ping"})
			// 서버 PING에 대한 PONG은 별도 처리 (간소화)
			conn.sendFrame(Frame{Type: PongFrame, Payload: "auto-pong"})
			conn.recordActivity()
			pingTimer.Reset(params.Time)

		case <-conn.done:
			return
		}
	}
}

// ========== EnforcementPolicy 검사 ==========
// grpc-go: internal/transport/http2_server.go handlePing 메서드
// 클라이언트가 너무 자주 PING을 보내면 GOAWAY로 연결을 끊는다.
func checkEnforcementPolicy(conn *Connection, policy EnforcementPolicy) bool {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	conn.pingCount++
	now := time.Now()

	// 첫 번째 PING이면 통과
	if conn.pingCount == 1 {
		conn.lastPingTime = now
		return true
	}

	elapsed := now.Sub(conn.lastPingTime)
	conn.lastPingTime = now

	// 활성 스트림 없이 PING이 허용되지 않으면 위반
	if !conn.hasStream && !policy.PermitWithoutStream {
		return false
	}

	// 최소 간격보다 짧으면 위반
	if elapsed < policy.MinTime {
		return false
	}

	return true
}

func main() {
	fmt.Println("========================================")
	fmt.Println("gRPC Keepalive 시뮬레이션")
	fmt.Println("========================================")

	// 1. 클라이언트 Keepalive — 정상 PING/PONG
	fmt.Println("\n[1] 클라이언트 Keepalive — 정상 동작")
	fmt.Println("──────────────────────────────────────")
	func() {
		conn := NewConnection("conn-1")
		conn.hasStream = true

		params := ClientParameters{
			Time:                50 * time.Millisecond,
			Timeout:             30 * time.Millisecond,
			PermitWithoutStream: false,
		}

		// PONG 응답기 (자동 응답)
		go func() {
			for {
				select {
				case f := <-conn.frames:
					if f.Type == PingFrame && f.Payload == "client-ping" {
						time.Sleep(5 * time.Millisecond)
						conn.sendFrame(Frame{Type: PongFrame, Payload: "pong"})
					}
				case <-conn.done:
					return
				}
			}
		}()

		go runClientKeepalive(conn, params)
		time.Sleep(180 * time.Millisecond)
		conn.close("테스트 종료")
		time.Sleep(10 * time.Millisecond)
		conn.printLog()
	}()

	// 2. 클라이언트 Keepalive — PONG 타임아웃
	fmt.Println("\n[2] 클라이언트 Keepalive — PONG 타임아웃")
	fmt.Println("──────────────────────────────────────────")
	func() {
		conn := NewConnection("conn-2")
		conn.hasStream = true

		params := ClientParameters{
			Time:                50 * time.Millisecond,
			Timeout:             20 * time.Millisecond,
			PermitWithoutStream: true,
		}

		// PONG 응답 없음 (타임아웃 시뮬레이션)
		go func() {
			for {
				select {
				case <-conn.frames:
					// PONG을 보내지 않음 — 타임아웃 유발
				case <-conn.done:
					return
				}
			}
		}()

		go runClientKeepalive(conn, params)
		time.Sleep(150 * time.Millisecond)
		conn.printLog()
	}()

	// 3. PermitWithoutStream = false (활성 스트림 없으면 PING 스킵)
	fmt.Println("\n[3] PermitWithoutStream = false")
	fmt.Println("─────────────────────────────────")
	func() {
		conn := NewConnection("conn-3")
		conn.hasStream = false // 활성 스트림 없음

		params := ClientParameters{
			Time:                50 * time.Millisecond,
			Timeout:             20 * time.Millisecond,
			PermitWithoutStream: false,
		}

		go runClientKeepalive(conn, params)
		time.Sleep(120 * time.Millisecond)
		conn.close("테스트 종료")
		time.Sleep(10 * time.Millisecond)
		conn.printLog()
	}()

	// 4. 서버 MaxConnectionIdle
	fmt.Println("\n[4] 서버 MaxConnectionIdle")
	fmt.Println("──────────────────────────")
	func() {
		conn := NewConnection("conn-4")
		conn.hasStream = false

		serverParams := ServerParameters{
			MaxConnectionIdle:     80 * time.Millisecond,
			MaxConnectionAge:      5 * time.Second, // 충분히 길게
			MaxConnectionAgeGrace: 10 * time.Millisecond,
			Time:                  200 * time.Millisecond,
			Timeout:               20 * time.Millisecond,
		}
		policy := EnforcementPolicy{
			MinTime:             50 * time.Millisecond,
			PermitWithoutStream: false,
		}

		go runServerKeepalive(conn, serverParams, policy)
		time.Sleep(150 * time.Millisecond)
		conn.printLog()
	}()

	// 5. 서버 MaxConnectionAge
	fmt.Println("\n[5] 서버 MaxConnectionAge")
	fmt.Println("─────────────────────────")
	func() {
		conn := NewConnection("conn-5")
		conn.hasStream = true

		serverParams := ServerParameters{
			MaxConnectionIdle:     5 * time.Second,
			MaxConnectionAge:      80 * time.Millisecond,
			MaxConnectionAgeGrace: 20 * time.Millisecond,
			Time:                  200 * time.Millisecond,
			Timeout:               20 * time.Millisecond,
		}
		policy := EnforcementPolicy{
			MinTime:             50 * time.Millisecond,
			PermitWithoutStream: true,
		}

		go runServerKeepalive(conn, serverParams, policy)
		time.Sleep(200 * time.Millisecond)
		conn.printLog()
	}()

	// 6. EnforcementPolicy — 핑 과다 감지
	fmt.Println("\n[6] EnforcementPolicy 검사")
	fmt.Println("──────────────────────────")
	func() {
		conn := NewConnection("conn-6")
		conn.hasStream = true

		policy := EnforcementPolicy{
			MinTime:             50 * time.Millisecond,
			PermitWithoutStream: false,
		}

		// 정상 간격 PING
		ok := checkEnforcementPolicy(conn, policy)
		fmt.Printf("  PING #1: 허용=%v (첫 번째 PING)\n", ok)

		time.Sleep(60 * time.Millisecond) // MinTime 초과
		ok = checkEnforcementPolicy(conn, policy)
		fmt.Printf("  PING #2: 허용=%v (60ms 후, MinTime=50ms 충족)\n", ok)

		time.Sleep(10 * time.Millisecond) // MinTime 미달
		ok = checkEnforcementPolicy(conn, policy)
		fmt.Printf("  PING #3: 허용=%v (10ms 후, MinTime=50ms 미달 → 위반!)\n", ok)

		// 활성 스트림 없이 PING
		conn.mu.Lock()
		conn.hasStream = false
		conn.mu.Unlock()
		time.Sleep(60 * time.Millisecond)
		ok = checkEnforcementPolicy(conn, policy)
		fmt.Printf("  PING #4: 허용=%v (스트림 없음, PermitWithoutStream=false → 위반!)\n", ok)

		conn.close("테스트 종료")
	}()

	// 7. 전체 동작 요약
	fmt.Println("\n[7] Keepalive 동작 요약")
	fmt.Println("───────────────────────")
	fmt.Println("  클라이언트 파라미터:")
	fmt.Println("    Time               : 활동 없을 때 PING 간격 (최소 10초)")
	fmt.Println("    Timeout            : PONG 대기 시간 (기본 20초)")
	fmt.Println("    PermitWithoutStream : 스트림 없이 PING 허용")
	fmt.Println()
	fmt.Println("  서버 파라미터:")
	fmt.Println("    MaxConnectionIdle     : 유휴 연결 GOAWAY까지 시간")
	fmt.Println("    MaxConnectionAge      : 연결 최대 수명 (±10% 지터)")
	fmt.Println("    MaxConnectionAgeGrace : GOAWAY 후 유예 시간")
	fmt.Println("    Time                  : 서버→클라이언트 PING 간격")
	fmt.Println("    Timeout               : PONG 대기 시간")
	fmt.Println()
	fmt.Println("  서버 정책 (EnforcementPolicy):")
	fmt.Println("    MinTime             : PING 최소 간격 (기본 5분)")
	fmt.Println("    PermitWithoutStream  : 스트림 없이 PING 허용 여부")

	fmt.Println("\n========================================")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("========================================")
}
