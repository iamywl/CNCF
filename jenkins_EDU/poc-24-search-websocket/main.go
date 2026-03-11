package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Jenkins Search + WebSocket Messaging 시뮬레이션
// =============================================================================
//
// Jenkins는 검색 시스템으로 잡/빌드/설정을 검색하고,
// WebSocket을 통해 실시간 빌드 로그/이벤트를 클라이언트에 전달한다.
//
// 핵심 개념:
//   - Search: 자동완성 기반 검색 (SuggestItem)
//   - SearchIndex: Trie 기반 검색 인덱스
//   - WebSocket: 양방향 실시간 통신 (빌드 콘솔, 이벤트)
//   - Channel: 주제별 메시지 구독
//
// 실제 코드 참조:
//   - core/src/main/java/hudson/search/: 검색 시스템
//   - core/src/main/java/jenkins/websocket/: WebSocket
// =============================================================================

// --- Search System ---

type SearchItem struct {
	Name        string
	URL         string
	Description string
	Type        string // job, build, view, user, plugin
}

type TrieNode struct {
	children map[rune]*TrieNode
	items    []SearchItem
	isEnd    bool
}

type SearchIndex struct {
	root *TrieNode
}

func NewSearchIndex() *SearchIndex {
	return &SearchIndex{root: &TrieNode{children: make(map[rune]*TrieNode)}}
}

func (si *SearchIndex) Add(item SearchItem) {
	// 이름을 소문자로 변환하여 인덱싱
	word := strings.ToLower(item.Name)
	node := si.root
	for _, ch := range word {
		if node.children[ch] == nil {
			node.children[ch] = &TrieNode{children: make(map[rune]*TrieNode)}
		}
		node = node.children[ch]
	}
	node.isEnd = true
	node.items = append(node.items, item)
}

// Search는 접두사 매칭으로 검색한다 (자동완성).
func (si *SearchIndex) Search(prefix string, maxResults int) []SearchItem {
	prefix = strings.ToLower(prefix)
	node := si.root

	// 접두사까지 이동
	for _, ch := range prefix {
		if node.children[ch] == nil {
			return nil
		}
		node = node.children[ch]
	}

	// 서브트리에서 모든 아이템 수집
	var results []SearchItem
	si.collectItems(node, &results, maxResults)
	return results
}

func (si *SearchIndex) collectItems(node *TrieNode, results *[]SearchItem, max int) {
	if len(*results) >= max {
		return
	}
	if node.isEnd {
		for _, item := range node.items {
			if len(*results) >= max {
				return
			}
			*results = append(*results, item)
		}
	}
	for _, child := range node.children {
		si.collectItems(child, results, max)
	}
}

// --- WebSocket ---

type WSMessageType string

const (
	WSConsoleLog WSMessageType = "console-log"
	WSBuildEvent WSMessageType = "build-event"
	WSQueueEvent WSMessageType = "queue-event"
	WSPing       WSMessageType = "ping"
	WSPong       WSMessageType = "pong"
)

type WSMessage struct {
	Type      WSMessageType `json:"type"`
	Channel   string        `json:"channel"`
	Data      string        `json:"data"`
	Timestamp time.Time     `json:"timestamp"`
}

func (m WSMessage) String() string {
	data := m.Data
	if len(data) > 60 {
		data = data[:60] + "..."
	}
	return fmt.Sprintf("[%s] %s/%s: %s", m.Timestamp.Format("15:04:05"), m.Type, m.Channel, data)
}

// WSClient는 WebSocket 클라이언트를 시뮬레이션한다.
type WSClient struct {
	ID          string
	Channels    map[string]bool
	MessageChan chan WSMessage
	Done        chan struct{}
	received    []WSMessage
	mu          sync.Mutex
}

func NewWSClient(id string) *WSClient {
	return &WSClient{
		ID:          id,
		Channels:    make(map[string]bool),
		MessageChan: make(chan WSMessage, 100),
		Done:        make(chan struct{}),
	}
}

func (c *WSClient) Subscribe(channel string) {
	c.Channels[channel] = true
}

func (c *WSClient) Unsubscribe(channel string) {
	delete(c.Channels, channel)
}

func (c *WSClient) Start() {
	go func() {
		for {
			select {
			case msg := <-c.MessageChan:
				c.mu.Lock()
				c.received = append(c.received, msg)
				c.mu.Unlock()
			case <-c.Done:
				return
			}
		}
	}()
}

func (c *WSClient) Stop() {
	close(c.Done)
}

func (c *WSClient) GetReceived() []WSMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]WSMessage{}, c.received...)
}

// WSServer는 WebSocket 서버를 시뮬레이션한다.
type WSServer struct {
	mu      sync.RWMutex
	clients map[string]*WSClient
}

func NewWSServer() *WSServer {
	return &WSServer{clients: make(map[string]*WSClient)}
}

func (s *WSServer) Register(client *WSClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[client.ID] = client
	client.Start()
}

func (s *WSServer) Unregister(clientID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if client, ok := s.clients[clientID]; ok {
		client.Stop()
		delete(s.clients, clientID)
	}
}

// Broadcast는 채널에 구독된 모든 클라이언트에 메시지를 전송한다.
func (s *WSServer) Broadcast(msg WSMessage) int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sent := 0
	for _, client := range s.clients {
		if client.Channels[msg.Channel] || msg.Channel == "*" {
			select {
			case client.MessageChan <- msg:
				sent++
			default:
				// 버퍼 가득 차면 스킵
			}
		}
	}
	return sent
}

func main() {
	fmt.Println("=== Jenkins Search + WebSocket 시뮬레이션 ===")
	fmt.Println()

	// --- Search Index 구축 ---
	fmt.Println("[1] Search Index 구축")
	fmt.Println(strings.Repeat("-", 60))

	index := NewSearchIndex()
	items := []SearchItem{
		{"my-app-build", "/job/my-app-build/", "Maven 빌드 파이프라인", "job"},
		{"my-app-deploy", "/job/my-app-deploy/", "배포 파이프라인", "job"},
		{"my-app-test", "/job/my-app-test/", "통합 테스트", "job"},
		{"microservice-api", "/job/microservice-api/", "API 서비스 빌드", "job"},
		{"microservice-web", "/job/microservice-web/", "웹 프론트엔드 빌드", "job"},
		{"master", "/computer/master/", "빌트인 노드", "node"},
		{"worker-1", "/computer/worker-1/", "빌드 에이전트 1", "node"},
		{"All", "/view/All/", "전체 잡 뷰", "view"},
		{"My Views", "/me/my-views/", "내 뷰", "view"},
		{"admin", "/user/admin/", "관리자 사용자", "user"},
		{"manage", "/manage/", "시스템 관리", "admin"},
		{"manage plugins", "/pluginManager/", "플러그인 관리", "admin"},
		{"manage credentials", "/credentials/", "자격증명 관리", "admin"},
		{"manage nodes", "/computer/", "노드 관리", "admin"},
	}

	for _, item := range items {
		index.Add(item)
		fmt.Printf("  Indexed: [%s] %s\n", item.Type, item.Name)
	}
	fmt.Println()

	// --- 검색 테스트 ---
	fmt.Println("[2] 검색 테스트 (자동완성)")
	fmt.Println(strings.Repeat("-", 60))

	queries := []string{"my", "micro", "manage", "work", "a", "xyz"}
	for _, q := range queries {
		results := index.Search(q, 5)
		fmt.Printf("  '%s' -> %d results:\n", q, len(results))
		for _, r := range results {
			fmt.Printf("    [%s] %s (%s) -> %s\n", r.Type, r.Name, r.Description, r.URL)
		}
	}
	fmt.Println()

	// --- WebSocket 서버 ---
	fmt.Println("[3] WebSocket 서버 초기화")
	fmt.Println(strings.Repeat("-", 60))

	server := NewWSServer()

	// 클라이언트 등록
	client1 := NewWSClient("browser-1")
	client1.Subscribe("build/my-app-build/42")
	client1.Subscribe("queue-events")
	server.Register(client1)
	fmt.Printf("  Client: %s (channels: build/my-app-build/42, queue-events)\n", client1.ID)

	client2 := NewWSClient("browser-2")
	client2.Subscribe("build/my-app-build/42")
	server.Register(client2)
	fmt.Printf("  Client: %s (channels: build/my-app-build/42)\n", client2.ID)

	client3 := NewWSClient("cli-1")
	client3.Subscribe("build/microservice-api/10")
	client3.Subscribe("queue-events")
	server.Register(client3)
	fmt.Printf("  Client: %s (channels: build/microservice-api/10, queue-events)\n", client3.ID)
	fmt.Println()

	// --- 빌드 콘솔 로그 스트리밍 ---
	fmt.Println("[4] 빌드 콘솔 로그 스트리밍")
	fmt.Println(strings.Repeat("-", 60))

	consoleLogs := []string{
		"[Pipeline] Start of Pipeline",
		"[Pipeline] node",
		"Running on worker-1 in /var/jenkins/workspace/my-app-build",
		"[Pipeline] { (Checkout)",
		"Cloning repository https://github.com/example/my-app.git",
		"Checking out branch: main",
		"[Pipeline] } (Checkout)",
		"[Pipeline] { (Build)",
		"$ mvn clean package -DskipTests",
		"[INFO] Building my-app 1.0.0-SNAPSHOT",
		"[INFO] BUILD SUCCESS",
		"[Pipeline] } (Build)",
		"[Pipeline] { (Test)",
		"$ mvn test",
		"Tests run: 42, Failures: 0, Errors: 0, Skipped: 2",
		"[Pipeline] } (Test)",
		"[Pipeline] End of Pipeline",
		"Finished: SUCCESS",
	}

	for _, logLine := range consoleLogs {
		msg := WSMessage{
			Type:      WSConsoleLog,
			Channel:   "build/my-app-build/42",
			Data:      logLine,
			Timestamp: time.Now(),
		}
		sent := server.Broadcast(msg)
		fmt.Printf("  -> %d clients: %s\n", sent, logLine)
		time.Sleep(10 * time.Millisecond)
	}
	fmt.Println()

	// --- 큐 이벤트 ---
	fmt.Println("[5] 큐 이벤트 브로드캐스트")
	fmt.Println(strings.Repeat("-", 60))

	queueEvents := []string{
		"Job 'microservice-web' entered queue",
		"Job 'microservice-web' assigned to worker-2",
		"Job 'microservice-web' left queue (started)",
		"Job 'my-app-deploy' entered queue",
		"Job 'my-app-deploy' waiting: no available executors",
	}

	for _, evt := range queueEvents {
		msg := WSMessage{
			Type:      WSQueueEvent,
			Channel:   "queue-events",
			Data:      evt,
			Timestamp: time.Now(),
		}
		sent := server.Broadcast(msg)
		fmt.Printf("  -> %d clients: %s\n", sent, evt)
	}
	fmt.Println()

	// --- 수신 확인 ---
	time.Sleep(100 * time.Millisecond)

	fmt.Println("[6] 클라이언트별 수신 메시지")
	fmt.Println(strings.Repeat("-", 60))

	for _, client := range []*WSClient{client1, client2, client3} {
		msgs := client.GetReceived()
		consoleCount := 0
		queueCount := 0
		for _, m := range msgs {
			switch m.Type {
			case WSConsoleLog:
				consoleCount++
			case WSQueueEvent:
				queueCount++
			}
		}
		fmt.Printf("  %s: total=%d (console=%d, queue=%d)\n",
			client.ID, len(msgs), consoleCount, queueCount)
	}
	fmt.Println()

	// --- Ping/Pong ---
	fmt.Println("[7] WebSocket Ping/Pong")
	fmt.Println(strings.Repeat("-", 60))
	pingMsg := WSMessage{Type: WSPing, Channel: "*", Data: "heartbeat", Timestamp: time.Now()}
	sent := server.Broadcast(pingMsg)
	fmt.Printf("  Ping sent to %d clients\n", sent)

	// 정리
	server.Unregister("browser-1")
	server.Unregister("browser-2")
	server.Unregister("cli-1")
	fmt.Println("  All clients disconnected")
	fmt.Println()

	_ = rand.New(rand.NewSource(time.Now().UnixNano()))
	fmt.Println("=== 시뮬레이션 완료 ===")
}
