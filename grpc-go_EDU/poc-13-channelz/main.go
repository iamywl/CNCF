// poc-13-channelz: gRPC 채널 진단 시스템 시뮬레이션
//
// grpc-go의 channelz 패키지 핵심 개념을 표준 라이브러리만으로 재현한다.
// - Channel, SubChannel, Socket, Server 엔티티
// - 메트릭 수집 (CallsStarted, CallsSucceeded, CallsFailed)
// - 자동 등록/해제
// - 조회 API 시뮬레이션
// - 이벤트 트레이스
//
// 실제 grpc-go 소스: internal/channelz/channel.go, subchannel.go, socket.go
package main

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ========== 엔티티 계층 구조 ==========
// grpc-go channelz 계층:
//   Server ─┬─ Socket (리스닝)
//            └─ Socket (연결)
//   Channel ─┬─ SubChannel ─── Socket
//             └─ SubChannel ─── Socket

// ========== 엔티티 타입 ==========
type EntityType int

const (
	ChannelEntity    EntityType = iota
	SubChannelEntity
	SocketEntity
	ServerEntity
)

func (e EntityType) String() string {
	switch e {
	case ChannelEntity:
		return "Channel"
	case SubChannelEntity:
		return "SubChannel"
	case SocketEntity:
		return "Socket"
	case ServerEntity:
		return "Server"
	default:
		return "Unknown"
	}
}

// ========== 채널 상태 ==========
// grpc-go: internal/channelz/types.go의 ChannelState
type ConnectivityState int

const (
	Idle ConnectivityState = iota
	Connecting
	Ready
	TransientFailure
	Shutdown
)

func (s ConnectivityState) String() string {
	names := []string{"IDLE", "CONNECTING", "READY", "TRANSIENT_FAILURE", "SHUTDOWN"}
	if int(s) < len(names) {
		return names[s]
	}
	return "UNKNOWN"
}

// ========== 메트릭 ==========
// grpc-go: internal/channelz/channel.go — ChannelMetrics
type ChannelMetrics struct {
	CallsStarted   int64
	CallsSucceeded int64
	CallsFailed    int64
	LastCallStartedTime time.Time
}

func (m *ChannelMetrics) String() string {
	return fmt.Sprintf("started=%d, succeeded=%d, failed=%d",
		m.CallsStarted, m.CallsSucceeded, m.CallsFailed)
}

// ========== 이벤트 트레이스 ==========
// grpc-go: internal/channelz/channel.go — ChannelTrace
type TraceEvent struct {
	Severity string // "INFO", "WARNING", "ERROR"
	Message  string
	Time     time.Time
	ChildRef int64 // 관련 자식 엔티티 ID
}

type ChannelTrace struct {
	mu             sync.Mutex
	Events         []TraceEvent
	CreationTime   time.Time
	EventsLogged   int64
	MaxEvents      int // 최대 이벤트 수 (링 버퍼)
}

func NewChannelTrace(maxEvents int) *ChannelTrace {
	return &ChannelTrace{
		CreationTime: time.Now(),
		MaxEvents:    maxEvents,
	}
}

func (t *ChannelTrace) AddEvent(severity, msg string, childRef int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	event := TraceEvent{
		Severity: severity,
		Message:  msg,
		Time:     time.Now(),
		ChildRef: childRef,
	}
	t.Events = append(t.Events, event)
	t.EventsLogged++

	// 링 버퍼: 최대 개수 초과 시 오래된 이벤트 제거
	if len(t.Events) > t.MaxEvents {
		t.Events = t.Events[1:]
	}
}

// ========== Channel ==========
// grpc-go: internal/channelz/channel.go 30행
type Channel struct {
	ID          int64
	RefName     string
	State       ConnectivityState
	Target      string
	Metrics     ChannelMetrics
	Trace       *ChannelTrace
	SubChannels map[int64]*SubChannel
	mu          sync.RWMutex
}

// ========== SubChannel ==========
// grpc-go: internal/channelz/subchannel.go 27행
type SubChannel struct {
	ID       int64
	RefName  string
	State    ConnectivityState
	Metrics  ChannelMetrics
	Trace    *ChannelTrace
	Sockets  map[int64]*Socket
	ParentID int64
	mu       sync.RWMutex
}

// ========== Socket ==========
// grpc-go: internal/channelz/socket.go 85행
type Socket struct {
	ID           int64
	RefName      string
	LocalAddr    string
	RemoteAddr   string
	StreamsStarted   int64
	StreamsSucceeded int64
	StreamsFailed    int64
	MessagesSent     int64
	MessagesRecv     int64
	BytesSent        int64
	BytesRecv        int64
	LastMsgSentTime  time.Time
	LastMsgRecvTime  time.Time
	ParentID         int64
}

// ========== Server ==========
type Server struct {
	ID      int64
	RefName string
	Metrics ChannelMetrics
	Trace   *ChannelTrace
	Sockets map[int64]*Socket
	mu      sync.RWMutex
}

// ========== ChannelMap (글로벌 레지스트리) ==========
// grpc-go: internal/channelz/channelmap.go
// 모든 channelz 엔티티를 관리하는 글로벌 맵이다.
type ChannelMap struct {
	mu          sync.RWMutex
	channels    map[int64]*Channel
	subChannels map[int64]*SubChannel
	sockets     map[int64]*Socket
	servers     map[int64]*Server
	idGen       int64
}

func NewChannelMap() *ChannelMap {
	return &ChannelMap{
		channels:    make(map[int64]*Channel),
		subChannels: make(map[int64]*SubChannel),
		sockets:     make(map[int64]*Socket),
		servers:     make(map[int64]*Server),
	}
}

func (cm *ChannelMap) nextID() int64 {
	return atomic.AddInt64(&cm.idGen, 1)
}

// RegisterChannel은 채널을 등록한다.
func (cm *ChannelMap) RegisterChannel(refName, target string) *Channel {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	ch := &Channel{
		ID:          cm.nextID(),
		RefName:     refName,
		State:       Idle,
		Target:      target,
		Trace:       NewChannelTrace(20),
		SubChannels: make(map[int64]*SubChannel),
	}
	cm.channels[ch.ID] = ch
	ch.Trace.AddEvent("INFO", fmt.Sprintf("Channel created: %s → %s", refName, target), 0)
	return ch
}

// RegisterSubChannel은 서브채널을 등록한다.
func (cm *ChannelMap) RegisterSubChannel(parent *Channel, refName string) *SubChannel {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	sc := &SubChannel{
		ID:       cm.nextID(),
		RefName:  refName,
		State:    Idle,
		Trace:    NewChannelTrace(20),
		Sockets:  make(map[int64]*Socket),
		ParentID: parent.ID,
	}
	cm.subChannels[sc.ID] = sc

	parent.mu.Lock()
	parent.SubChannels[sc.ID] = sc
	parent.mu.Unlock()

	parent.Trace.AddEvent("INFO", fmt.Sprintf("SubChannel created: %s", refName), sc.ID)
	return sc
}

// RegisterSocket은 소켓을 등록한다.
func (cm *ChannelMap) RegisterSocket(parent *SubChannel, localAddr, remoteAddr string) *Socket {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	sock := &Socket{
		ID:         cm.nextID(),
		RefName:    fmt.Sprintf("socket-%d", cm.idGen),
		LocalAddr:  localAddr,
		RemoteAddr: remoteAddr,
		ParentID:   parent.ID,
	}
	cm.sockets[sock.ID] = sock

	parent.mu.Lock()
	parent.Sockets[sock.ID] = sock
	parent.mu.Unlock()

	return sock
}

// RegisterServer는 서버를 등록한다.
func (cm *ChannelMap) RegisterServer(refName string) *Server {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	srv := &Server{
		ID:      cm.nextID(),
		RefName: refName,
		Trace:   NewChannelTrace(20),
		Sockets: make(map[int64]*Socket),
	}
	cm.servers[srv.ID] = srv
	srv.Trace.AddEvent("INFO", fmt.Sprintf("Server created: %s", refName), 0)
	return srv
}

// Unregister는 엔티티를 해제한다.
func (cm *ChannelMap) Unregister(entityType EntityType, id int64) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	switch entityType {
	case ChannelEntity:
		delete(cm.channels, id)
	case SubChannelEntity:
		delete(cm.subChannels, id)
	case SocketEntity:
		delete(cm.sockets, id)
	case ServerEntity:
		delete(cm.servers, id)
	}
}

// ========== 조회 API ==========
// grpc-go: channelz 서비스의 GetChannel, GetSubchannel 등 RPC

func (cm *ChannelMap) GetTopChannels() []*Channel {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	result := make([]*Channel, 0, len(cm.channels))
	for _, ch := range cm.channels {
		result = append(result, ch)
	}
	return result
}

func (cm *ChannelMap) GetServers() []*Server {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	result := make([]*Server, 0, len(cm.servers))
	for _, srv := range cm.servers {
		result = append(result, srv)
	}
	return result
}

func (cm *ChannelMap) GetChannel(id int64) (*Channel, bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	ch, ok := cm.channels[id]
	return ch, ok
}

// ========== RPC 메트릭 기록 ==========
func recordCallStart(ch *Channel, sc *SubChannel) {
	now := time.Now()
	ch.mu.Lock()
	ch.Metrics.CallsStarted++
	ch.Metrics.LastCallStartedTime = now
	ch.mu.Unlock()
	sc.mu.Lock()
	sc.Metrics.CallsStarted++
	sc.Metrics.LastCallStartedTime = now
	sc.mu.Unlock()
}

func recordCallSuccess(ch *Channel, sc *SubChannel) {
	ch.mu.Lock()
	ch.Metrics.CallsSucceeded++
	ch.mu.Unlock()
	sc.mu.Lock()
	sc.Metrics.CallsSucceeded++
	sc.mu.Unlock()
}

func recordCallFailure(ch *Channel, sc *SubChannel) {
	ch.mu.Lock()
	ch.Metrics.CallsFailed++
	ch.mu.Unlock()
	sc.mu.Lock()
	sc.Metrics.CallsFailed++
	sc.mu.Unlock()
}

// ========== 출력 헬퍼 ==========
func printChannel(ch *Channel, indent int) {
	prefix := strings.Repeat("  ", indent)
	fmt.Printf("%s[Channel #%d] %s → %s (state=%s)\n", prefix, ch.ID, ch.RefName, ch.Target, ch.State)
	fmt.Printf("%s  메트릭: %s\n", prefix, ch.Metrics.String())

	ch.mu.RLock()
	for _, sc := range ch.SubChannels {
		printSubChannel(sc, indent+1)
	}
	ch.mu.RUnlock()
}

func printSubChannel(sc *SubChannel, indent int) {
	prefix := strings.Repeat("  ", indent)
	fmt.Printf("%s[SubChannel #%d] %s (state=%s)\n", prefix, sc.ID, sc.RefName, sc.State)
	fmt.Printf("%s  메트릭: %s\n", prefix, sc.Metrics.String())

	sc.mu.RLock()
	for _, sock := range sc.Sockets {
		printSocket(sock, indent+1)
	}
	sc.mu.RUnlock()
}

func printSocket(sock *Socket, indent int) {
	prefix := strings.Repeat("  ", indent)
	fmt.Printf("%s[Socket #%d] %s → %s\n", prefix, sock.ID, sock.LocalAddr, sock.RemoteAddr)
	fmt.Printf("%s  streams: %d/%d/%d, msgs: sent=%d recv=%d, bytes: sent=%d recv=%d\n",
		prefix, sock.StreamsStarted, sock.StreamsSucceeded, sock.StreamsFailed,
		sock.MessagesSent, sock.MessagesRecv, sock.BytesSent, sock.BytesRecv)
}

func printTrace(trace *ChannelTrace, indent int) {
	prefix := strings.Repeat("  ", indent)
	trace.mu.Lock()
	defer trace.mu.Unlock()
	fmt.Printf("%s이벤트 트레이스 (총 %d개 기록, 표시 %d개):\n", prefix, trace.EventsLogged, len(trace.Events))
	for _, ev := range trace.Events {
		elapsed := ev.Time.Sub(trace.CreationTime).Truncate(time.Millisecond)
		childStr := ""
		if ev.ChildRef > 0 {
			childStr = fmt.Sprintf(" (ref=#%d)", ev.ChildRef)
		}
		fmt.Printf("%s  [%v] [%s] %s%s\n", prefix, elapsed, ev.Severity, ev.Message, childStr)
	}
}

func main() {
	fmt.Println("========================================")
	fmt.Println("gRPC Channelz 시뮬레이션")
	fmt.Println("========================================")

	cm := NewChannelMap()

	// 1. 채널 계층 구조 생성
	fmt.Println("\n[1] 채널 계층 구조 생성")
	fmt.Println("────────────────────────")

	// 서버 등록
	srv := cm.RegisterServer("grpc-server:50051")
	fmt.Printf("  서버 등록: #%d %s\n", srv.ID, srv.RefName)

	// 채널 등록 (클라이언트 측)
	ch := cm.RegisterChannel("grpc-channel", "dns:///myservice.example.com:443")
	fmt.Printf("  채널 등록: #%d %s → %s\n", ch.ID, ch.RefName, ch.Target)

	// 서브채널 등록 (백엔드 별)
	sc1 := cm.RegisterSubChannel(ch, "subchannel-10.0.0.1:443")
	sc2 := cm.RegisterSubChannel(ch, "subchannel-10.0.0.2:443")
	fmt.Printf("  서브채널 등록: #%d %s\n", sc1.ID, sc1.RefName)
	fmt.Printf("  서브채널 등록: #%d %s\n", sc2.ID, sc2.RefName)

	// 소켓 등록
	sock1 := cm.RegisterSocket(sc1, "192.168.1.100:54321", "10.0.0.1:443")
	sock2 := cm.RegisterSocket(sc2, "192.168.1.100:54322", "10.0.0.2:443")
	fmt.Printf("  소켓 등록: #%d %s → %s\n", sock1.ID, sock1.LocalAddr, sock1.RemoteAddr)
	fmt.Printf("  소켓 등록: #%d %s → %s\n", sock2.ID, sock2.LocalAddr, sock2.RemoteAddr)

	// 2. 상태 변경 시뮬레이션
	fmt.Println("\n[2] 상태 변경 시뮬레이션")
	fmt.Println("─────────────────────────")

	states := []ConnectivityState{Connecting, Ready}
	for _, state := range states {
		ch.State = state
		sc1.State = state
		sc2.State = state
		ch.Trace.AddEvent("INFO", fmt.Sprintf("State changed to %s", state), 0)
		fmt.Printf("  채널 상태: %s\n", state)
		time.Sleep(5 * time.Millisecond) // 시간차를 위해
	}

	// 3. RPC 호출 시뮬레이션 — 메트릭 수집
	fmt.Println("\n[3] RPC 호출 메트릭 수집")
	fmt.Println("─────────────────────────")

	// 성공적인 RPC 5회
	for i := 0; i < 5; i++ {
		targetSC := sc1
		if i%2 == 1 {
			targetSC = sc2
		}
		recordCallStart(ch, targetSC)
		time.Sleep(2 * time.Millisecond)
		recordCallSuccess(ch, targetSC)

		// 소켓 메트릭 업데이트
		if targetSC == sc1 {
			sock1.StreamsStarted++
			sock1.StreamsSucceeded++
			sock1.MessagesSent += 2
			sock1.MessagesRecv += 1
			sock1.BytesSent += 256
			sock1.BytesRecv += 512
		} else {
			sock2.StreamsStarted++
			sock2.StreamsSucceeded++
			sock2.MessagesSent += 2
			sock2.MessagesRecv += 1
			sock2.BytesSent += 128
			sock2.BytesRecv += 256
		}
	}

	// 실패한 RPC 2회
	for i := 0; i < 2; i++ {
		recordCallStart(ch, sc1)
		time.Sleep(1 * time.Millisecond)
		recordCallFailure(ch, sc1)
		ch.Trace.AddEvent("WARNING", fmt.Sprintf("RPC failed: deadline exceeded (call #%d)", i+1), sc1.ID)
		sock1.StreamsStarted++
		sock1.StreamsFailed++
	}

	fmt.Printf("  총 RPC: started=%d, succeeded=%d, failed=%d\n",
		ch.Metrics.CallsStarted, ch.Metrics.CallsSucceeded, ch.Metrics.CallsFailed)
	fmt.Printf("  sc1 메트릭: %s\n", sc1.Metrics.String())
	fmt.Printf("  sc2 메트릭: %s\n", sc2.Metrics.String())

	// 4. 전체 채널 트리 조회
	fmt.Println("\n[4] 채널 트리 조회 (GetTopChannels)")
	fmt.Println("────────────────────────────────────")
	for _, ch := range cm.GetTopChannels() {
		printChannel(ch, 1)
	}

	// 5. 서버 조회
	fmt.Println("\n[5] 서버 조회 (GetServers)")
	fmt.Println("───────────────────────────")
	for _, s := range cm.GetServers() {
		fmt.Printf("  [Server #%d] %s\n", s.ID, s.RefName)
		fmt.Printf("    메트릭: %s\n", s.Metrics.String())
	}

	// 6. 이벤트 트레이스 조회
	fmt.Println("\n[6] 채널 이벤트 트레이스")
	fmt.Println("─────────────────────────")
	printTrace(ch.Trace, 1)

	// 7. 엔티티 해제
	fmt.Println("\n[7] 엔티티 해제")
	fmt.Println("──────────────────")
	cm.Unregister(SocketEntity, sock2.ID)
	fmt.Printf("  소켓 #%d 해제 완료\n", sock2.ID)
	cm.Unregister(SubChannelEntity, sc2.ID)
	fmt.Printf("  서브채널 #%d 해제 완료\n", sc2.ID)

	fmt.Println("\n  해제 후 채널 트리:")
	for _, ch := range cm.GetTopChannels() {
		printChannel(ch, 2)
	}

	fmt.Println("\n========================================")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("========================================")
}
