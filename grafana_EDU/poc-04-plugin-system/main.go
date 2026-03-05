package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 1. 플러그인 상태 및 타입 정의
// ---------------------------------------------------------------------------

// PluginState — 플러그인 라이프사이클 상태
type PluginState int

const (
	StateNotStarted PluginState = iota
	StateStartInit
	StateStartSuccess
	StateStartFailed
	StateStopped
)

func (s PluginState) String() string {
	switch s {
	case StateNotStarted:
		return "NotStarted"
	case StateStartInit:
		return "StartInit"
	case StateStartSuccess:
		return "StartSuccess"
	case StateStartFailed:
		return "StartFailed"
	case StateStopped:
		return "Stopped"
	default:
		return "Unknown"
	}
}

// PluginType — 플러그인 타입
type PluginType string

const (
	PluginTypeDatasource PluginType = "datasource"
	PluginTypePanel      PluginType = "panel"
	PluginTypeApp        PluginType = "app"
)

// ---------------------------------------------------------------------------
// 2. 플러그인 인터페이스
// ---------------------------------------------------------------------------

// HealthStatus — 헬스 체크 상태
type HealthStatus int

const (
	HealthOK      HealthStatus = iota
	HealthError
	HealthUnknown
)

func (h HealthStatus) String() string {
	switch h {
	case HealthOK:
		return "OK"
	case HealthError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// HealthResult — 헬스 체크 결과
type HealthResult struct {
	Status  HealthStatus
	Message string
}

// QueryRequest — RPC 쿼리 요청
type QueryRequest struct {
	RefID string
	Query string
}

// QueryResponse — RPC 쿼리 응답
type QueryResponse struct {
	RefID  string
	Data   string
	Error  error
}

// Plugin — 플러그인이 구현해야 하는 인터페이스
type Plugin interface {
	PluginID() string
	PluginType() PluginType
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	QueryData(ctx context.Context, req *QueryRequest) (*QueryResponse, error)
	CheckHealth(ctx context.Context) (*HealthResult, error)
}

// ---------------------------------------------------------------------------
// 3. RPC Channel — gRPC 통신 시뮬레이션
// ---------------------------------------------------------------------------

// RPCMessage — gRPC 메시지 시뮬레이션
type RPCMessage struct {
	Method   string
	Request  interface{}
	Response chan interface{}
	ErrChan  chan error
}

// RPCChannel — gRPC 유사 채널
type RPCChannel struct {
	messages chan RPCMessage
	done     chan struct{}
}

// NewRPCChannel — RPC 채널 생성
func NewRPCChannel() *RPCChannel {
	return &RPCChannel{
		messages: make(chan RPCMessage, 10),
		done:     make(chan struct{}),
	}
}

// Call — RPC 호출 (클라이언트 측)
func (ch *RPCChannel) Call(method string, req interface{}) (interface{}, error) {
	msg := RPCMessage{
		Method:   method,
		Request:  req,
		Response: make(chan interface{}, 1),
		ErrChan:  make(chan error, 1),
	}

	select {
	case ch.messages <- msg:
	case <-ch.done:
		return nil, fmt.Errorf("RPC channel closed")
	}

	select {
	case resp := <-msg.Response:
		return resp, nil
	case err := <-msg.ErrChan:
		return nil, err
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("RPC call timeout: %s", method)
	}
}

// Close — RPC 채널 종료
func (ch *RPCChannel) Close() {
	close(ch.done)
}

// ---------------------------------------------------------------------------
// 4. Backend Plugin 구현
// ---------------------------------------------------------------------------

// BackendPlugin — Grafana Backend Plugin 시뮬레이션
type BackendPlugin struct {
	id         string
	pluginType PluginType
	state      PluginState
	rpcChan    *RPCChannel
	mu         sync.RWMutex
	startedAt  time.Time
	version    string
	signature  string // "valid", "invalid", "unsigned"
}

// NewBackendPlugin — 새 Backend Plugin 생성
func NewBackendPlugin(id string, pType PluginType, version, signature string) *BackendPlugin {
	return &BackendPlugin{
		id:         id,
		pluginType: pType,
		state:      StateNotStarted,
		rpcChan:    NewRPCChannel(),
		version:    version,
		signature:  signature,
	}
}

func (p *BackendPlugin) PluginID() string     { return p.id }
func (p *BackendPlugin) PluginType() PluginType { return p.pluginType }

func (p *BackendPlugin) State() PluginState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

func (p *BackendPlugin) setState(s PluginState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.state
	p.state = s
	fmt.Printf("    [%s] 상태 변경: %s → %s\n", p.id, old, s)
}

// Start — 플러그인 프로세스 시작 (gRPC 서버 시뮬레이션)
func (p *BackendPlugin) Start(ctx context.Context) error {
	p.setState(StateStartInit)

	// 백그라운드 RPC 서버 시뮬레이션
	go func() {
		for {
			select {
			case msg, ok := <-p.rpcChan.messages:
				if !ok {
					return
				}
				p.handleRPC(msg)
			case <-p.rpcChan.done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	// 초기화 시간 시뮬레이션
	time.Sleep(50 * time.Millisecond)

	p.startedAt = time.Now()
	p.setState(StateStartSuccess)
	return nil
}

// handleRPC — RPC 요청 처리
func (p *BackendPlugin) handleRPC(msg RPCMessage) {
	switch msg.Method {
	case "QueryData":
		req := msg.Request.(*QueryRequest)
		resp := &QueryResponse{
			RefID: req.RefID,
			Data:  fmt.Sprintf("[%s] result for: %s", p.id, req.Query),
		}
		msg.Response <- resp
	case "CheckHealth":
		msg.Response <- &HealthResult{Status: HealthOK, Message: "Plugin is running"}
	default:
		msg.ErrChan <- fmt.Errorf("unknown method: %s", msg.Method)
	}
}

// Stop — 플러그인 종료
func (p *BackendPlugin) Stop(ctx context.Context) error {
	fmt.Printf("    [%s] Graceful shutdown 시작...\n", p.id)
	p.rpcChan.Close()

	// 종료 대기 (타임아웃 포함)
	select {
	case <-time.After(100 * time.Millisecond):
		fmt.Printf("    [%s] 종료 완료\n", p.id)
	case <-ctx.Done():
		fmt.Printf("    [%s] 강제 종료 (타임아웃)\n", p.id)
	}

	p.setState(StateStopped)
	return nil
}

// QueryData — RPC를 통한 쿼리 실행
func (p *BackendPlugin) QueryData(ctx context.Context, req *QueryRequest) (*QueryResponse, error) {
	if p.State() != StateStartSuccess {
		return nil, fmt.Errorf("plugin %s is not running (state: %s)", p.id, p.State())
	}

	result, err := p.rpcChan.Call("QueryData", req)
	if err != nil {
		return nil, err
	}
	return result.(*QueryResponse), nil
}

// CheckHealth — RPC를 통한 헬스 체크
func (p *BackendPlugin) CheckHealth(ctx context.Context) (*HealthResult, error) {
	if p.State() != StateStartSuccess {
		return &HealthResult{Status: HealthError, Message: "Plugin not running"}, nil
	}

	result, err := p.rpcChan.Call("CheckHealth", nil)
	if err != nil {
		return &HealthResult{Status: HealthError, Message: err.Error()}, nil
	}
	return result.(*HealthResult), nil
}

// ---------------------------------------------------------------------------
// 5. Plugin Registry
// ---------------------------------------------------------------------------

// PluginRegistry — 플러그인 저장소
type PluginRegistry struct {
	plugins map[string]Plugin
	mu      sync.RWMutex
}

// NewPluginRegistry — 레지스트리 생성
func NewPluginRegistry() *PluginRegistry {
	return &PluginRegistry{
		plugins: make(map[string]Plugin),
	}
}

// Register — 플러그인 등록
func (r *PluginRegistry) Register(p Plugin) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.plugins[p.PluginID()]; exists {
		return fmt.Errorf("plugin already registered: %s", p.PluginID())
	}
	r.plugins[p.PluginID()] = p
	fmt.Printf("  [Registry] 플러그인 등록: %s (type: %s)\n", p.PluginID(), p.PluginType())
	return nil
}

// Get — 플러그인 조회
func (r *PluginRegistry) Get(id string) (Plugin, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.plugins[id]
	return p, ok
}

// List — 전체 플러그인 목록
func (r *PluginRegistry) List() []Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Plugin, 0, len(r.plugins))
	for _, p := range r.plugins {
		result = append(result, p)
	}
	return result
}

// ---------------------------------------------------------------------------
// 6. Plugin Manager — 라이프사이클 관리
// ---------------------------------------------------------------------------

// PluginManager — 플러그인 라이프사이클 매니저
type PluginManager struct {
	registry *PluginRegistry
}

// NewPluginManager — 매니저 생성
func NewPluginManager(registry *PluginRegistry) *PluginManager {
	return &PluginManager{registry: registry}
}

// RunPipeline — 전체 파이프라인 실행: Discovery → Bootstrap → Init → Validation
func (m *PluginManager) RunPipeline(ctx context.Context, plugins []*BackendPlugin) error {
	// Phase 1: Discovery
	fmt.Println("\n  === Phase 1: Discovery (플러그인 탐색) ===")
	for _, p := range plugins {
		fmt.Printf("    발견: %s (type: %s, version: %s)\n", p.PluginID(), p.PluginType(), p.version)
	}

	// Phase 2: Bootstrap (메타데이터 로드)
	fmt.Println("\n  === Phase 2: Bootstrap (메타데이터 로드) ===")
	for _, p := range plugins {
		fmt.Printf("    [%s] plugin.json 로드 완료\n", p.PluginID())
		fmt.Printf("    [%s] 의존성 확인 완료\n", p.PluginID())
	}

	// Phase 3: Validation (서명 검증)
	fmt.Println("\n  === Phase 3: Validation (서명 검증) ===")
	validPlugins := make([]*BackendPlugin, 0)
	for _, p := range plugins {
		switch p.signature {
		case "valid":
			fmt.Printf("    [%s] 서명 검증 통과 (Grafana Labs 서명)\n", p.PluginID())
			validPlugins = append(validPlugins, p)
		case "community":
			fmt.Printf("    [%s] 커뮤니티 플러그인 (서명 없음, allow_loading_unsigned_plugins 확인)\n", p.PluginID())
			validPlugins = append(validPlugins, p)
		case "invalid":
			fmt.Printf("    [%s] 서명 검증 실패! 로드 거부됨\n", p.PluginID())
		default:
			fmt.Printf("    [%s] 서명 상태 불명, 로드 허용\n", p.PluginID())
			validPlugins = append(validPlugins, p)
		}
	}

	// Phase 4: Registration
	fmt.Println("\n  === Phase 4: Registration (레지스트리 등록) ===")
	for _, p := range validPlugins {
		if err := m.registry.Register(p); err != nil {
			fmt.Printf("    등록 실패: %v\n", err)
		}
	}

	// Phase 5: Initialization (플러그인 시작)
	fmt.Println("\n  === Phase 5: Initialization (플러그인 시작) ===")
	for _, p := range validPlugins {
		if err := p.Start(ctx); err != nil {
			fmt.Printf("    [%s] 시작 실패: %v\n", p.PluginID(), err)
		}
	}

	return nil
}

// Shutdown — 모든 플러그인 종료
func (m *PluginManager) Shutdown(ctx context.Context) {
	fmt.Println("\n  === Shutdown (전체 플러그인 종료) ===")

	shutdownCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	for _, p := range m.registry.List() {
		if err := p.Stop(shutdownCtx); err != nil {
			fmt.Printf("    [%s] 종료 에러: %v\n", p.PluginID(), err)
		}
	}
}

// ---------------------------------------------------------------------------
// 7. 메인
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("==================================================")
	fmt.Println("  Grafana Plugin Lifecycle Simulation")
	fmt.Println("==================================================")

	ctx := context.Background()

	// 플러그인 정의
	plugins := []*BackendPlugin{
		NewBackendPlugin("prometheus", PluginTypeDatasource, "10.0.0", "valid"),
		NewBackendPlugin("loki", PluginTypeDatasource, "10.0.0", "valid"),
		NewBackendPlugin("custom-panel", PluginTypePanel, "1.2.0", "community"),
		NewBackendPlugin("malicious-plugin", PluginTypeApp, "0.1.0", "invalid"),
	}

	// 레지스트리 및 매니저 생성
	registry := NewPluginRegistry()
	manager := NewPluginManager(registry)

	// 전체 파이프라인 실행
	if err := manager.RunPipeline(ctx, plugins); err != nil {
		fmt.Printf("파이프라인 에러: %v\n", err)
		return
	}

	// 플러그인 상태 확인
	fmt.Println("\n--- 플러그인 상태 ---")
	fmt.Printf("  %-20s %-12s %-15s\n", "Plugin ID", "Type", "State")
	fmt.Println("  " + strings.Repeat("-", 47))
	for _, p := range registry.List() {
		bp := p.(*BackendPlugin)
		fmt.Printf("  %-20s %-12s %-15s\n", bp.PluginID(), bp.PluginType(), bp.State())
	}

	// 헬스 체크
	fmt.Println("\n--- 헬스 체크 ---")
	for _, p := range registry.List() {
		health, err := p.CheckHealth(ctx)
		if err != nil {
			fmt.Printf("  [%s] 헬스 체크 에러: %v\n", p.PluginID(), err)
		} else {
			fmt.Printf("  [%s] 상태: %s, 메시지: %s\n", p.PluginID(), health.Status, health.Message)
		}
	}

	// RPC 쿼리 테스트
	fmt.Println("\n--- RPC 쿼리 테스트 ---")
	for _, pluginID := range []string{"prometheus", "loki", "custom-panel"} {
		p, ok := registry.Get(pluginID)
		if !ok {
			fmt.Printf("  [%s] 플러그인을 찾을 수 없음\n", pluginID)
			continue
		}

		resp, err := p.QueryData(ctx, &QueryRequest{
			RefID: "A",
			Query: fmt.Sprintf("test query for %s", pluginID),
		})
		if err != nil {
			fmt.Printf("  [%s] 쿼리 에러: %v\n", pluginID, err)
		} else {
			fmt.Printf("  [%s] 응답: RefID=%s, Data=%s\n", pluginID, resp.RefID, resp.Data)
		}
	}

	// 미등록 플러그인 쿼리 시도
	fmt.Println("\n--- 미등록 플러그인 접근 테스트 ---")
	if _, ok := registry.Get("malicious-plugin"); !ok {
		fmt.Println("  [malicious-plugin] 레지스트리에 없음 (서명 검증 실패로 로드 거부됨)")
	}

	// Graceful Shutdown
	manager.Shutdown(ctx)

	// 종료 후 상태 확인
	fmt.Println("\n--- 종료 후 상태 ---")
	for _, p := range registry.List() {
		bp := p.(*BackendPlugin)
		fmt.Printf("  [%s] 최종 상태: %s\n", bp.PluginID(), bp.State())
	}

	// 아키텍처 요약
	fmt.Println("\n--- Grafana 플러그인 아키텍처 ---")
	fmt.Println(`
  ┌──────────────────────────────────────────────────┐
  │                Grafana Server                     │
  │                                                   │
  │  ┌─────────────┐    ┌──────────────────────┐     │
  │  │ PluginMgr   │───▶│   PluginRegistry     │     │
  │  │             │    │   ┌──────────────┐   │     │
  │  │ Discovery   │    │   │ prometheus   │   │     │
  │  │ Bootstrap   │    │   │ loki         │   │     │
  │  │ Init        │    │   │ mysql        │   │     │
  │  │ Validation  │    │   └──────────────┘   │     │
  │  └─────────────┘    └──────────────────────┘     │
  │         │                      │                  │
  │         │                 gRPC (channels)         │
  │         │                      │                  │
  │         ▼                      ▼                  │
  │  ┌─────────────┐    ┌──────────────────────┐     │
  │  │ Lifecycle   │    │  Backend Plugin       │     │
  │  │             │    │  (별도 프로세스)         │     │
  │  │ Start()     │    │  ┌─────────────────┐  │     │
  │  │ Stop()      │    │  │ QueryData()     │  │     │
  │  │ Health()    │    │  │ CheckHealth()   │  │     │
  │  └─────────────┘    │  │ CallResource()  │  │     │
  │                     │  └─────────────────┘  │     │
  │                     └──────────────────────┘     │
  └──────────────────────────────────────────────────┘
`)
}
