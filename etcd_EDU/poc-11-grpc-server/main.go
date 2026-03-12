// poc-11-grpc-server: etcd gRPC 서버 시뮬레이션 (net/http + JSON)
//
// etcd는 gRPC로 KV/Watch/Lease 서비스를 제공한다.
// 실제 구현 (server/etcdserver/api/v3rpc/):
// - kv.go: KV 서비스 (Range, Put, DeleteRange, Txn, Compact)
// - watch.go: Watch 서비스 (양방향 스트리밍)
// - 모든 응답에 ResponseHeader 포함 (cluster_id, member_id, revision, raft_term)
//
// 이 PoC는 gRPC 대신 net/http + JSON으로 동일한 API를 시뮬레이션한다.
// Watch는 SSE(Server-Sent Events) 방식으로 구현한다.
//
// 사용법: go run main.go
// 테스트: curl로 Put/Get/Watch 요청 (프로그램 내 자동 테스트)

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ===== ResponseHeader =====
// etcd의 모든 gRPC 응답에 포함되는 헤더
// api/etcdserverpb/rpc.proto의 ResponseHeader 참조

type ResponseHeader struct {
	ClusterID uint64 `json:"cluster_id"`
	MemberID  uint64 `json:"member_id"`
	Revision  int64  `json:"revision"`
	RaftTerm  uint64 `json:"raft_term"`
}

// ===== KeyValue =====

type KeyValue struct {
	Key            string `json:"key"`
	Value          string `json:"value"`
	CreateRevision int64  `json:"create_revision"`
	ModRevision    int64  `json:"mod_revision"`
	Version        int64  `json:"version"`
}

// ===== 요청/응답 구조체 =====

// PUT /v3/kv/put
type PutRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type PutResponse struct {
	Header *ResponseHeader `json:"header"`
}

// POST /v3/kv/range
type RangeRequest struct {
	Key      string `json:"key"`
	RangeEnd string `json:"range_end,omitempty"`
}

type RangeResponse struct {
	Header *ResponseHeader `json:"header"`
	Kvs    []*KeyValue     `json:"kvs"`
	Count  int64           `json:"count"`
}

// POST /v3/kv/deleterange
type DeleteRangeRequest struct {
	Key string `json:"key"`
}

type DeleteRangeResponse struct {
	Header  *ResponseHeader `json:"header"`
	Deleted int64           `json:"deleted"`
}

// Watch 이벤트
type WatchEvent struct {
	Type string   `json:"type"` // PUT 또는 DELETE
	Kv   *KeyValue `json:"kv"`
}

type WatchResponse struct {
	Header  *ResponseHeader `json:"header"`
	Events  []*WatchEvent   `json:"events"`
	WatchID int64           `json:"watch_id"`
}

// ===== MVCC Store =====
// 간단한 MVCC 스토어 (이전 PoC와 유사하지만 서버 연동에 집중)

type mvccStore struct {
	mu         sync.RWMutex
	data       map[string]*KeyValue
	revision   int64
	watchers   map[int64]chan *WatchEvent
	watcherSeq int64
}

func newMVCCStore() *mvccStore {
	return &mvccStore{
		data:     make(map[string]*KeyValue),
		watchers: make(map[int64]chan *WatchEvent),
	}
}

func (s *mvccStore) put(key, value string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.revision++

	existing, exists := s.data[key]
	var createRev int64
	var ver int64
	if exists {
		createRev = existing.CreateRevision
		ver = existing.Version
	} else {
		createRev = s.revision
	}
	ver++

	kv := &KeyValue{
		Key:            key,
		Value:          value,
		CreateRevision: createRev,
		ModRevision:    s.revision,
		Version:        ver,
	}
	s.data[key] = kv

	// Watch 이벤트 전송
	event := &WatchEvent{Type: "PUT", Kv: kv}
	for _, ch := range s.watchers {
		select {
		case ch <- event:
		default: // 채널 가득 차면 스킵
		}
	}

	return s.revision
}

func (s *mvccStore) get(key string) (*KeyValue, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	kv, ok := s.data[key]
	return kv, ok
}

func (s *mvccStore) rangeGet(prefix string) []*KeyValue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*KeyValue
	for k, v := range s.data {
		if strings.HasPrefix(k, prefix) {
			result = append(result, v)
		}
	}
	return result
}

func (s *mvccStore) delete(key string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	kv, ok := s.data[key]
	if !ok {
		return s.revision, false
	}

	s.revision++
	delete(s.data, key)

	// Watch 이벤트 전송
	event := &WatchEvent{
		Type: "DELETE",
		Kv: &KeyValue{
			Key:         key,
			ModRevision: s.revision,
			Version:     kv.Version,
		},
	}
	for _, ch := range s.watchers {
		select {
		case ch <- event:
		default:
		}
	}

	return s.revision, true
}

func (s *mvccStore) currentRevision() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revision
}

func (s *mvccStore) addWatcher() (int64, chan *WatchEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watcherSeq++
	ch := make(chan *WatchEvent, 64)
	s.watchers[s.watcherSeq] = ch
	return s.watcherSeq, ch
}

func (s *mvccStore) removeWatcher(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.watchers[id]; ok {
		close(ch)
		delete(s.watchers, id)
	}
}

// ===== EtcdServer =====
// etcd gRPC 서버를 HTTP + JSON으로 시뮬레이션

type EtcdServer struct {
	store     *mvccStore
	clusterID uint64
	memberID  uint64
	raftTerm  uint64
	server    *http.Server
}

func NewEtcdServer(clusterID, memberID, raftTerm uint64) *EtcdServer {
	return &EtcdServer{
		store:     newMVCCStore(),
		clusterID: clusterID,
		memberID:  memberID,
		raftTerm:  raftTerm,
	}
}

func (es *EtcdServer) makeHeader() *ResponseHeader {
	return &ResponseHeader{
		ClusterID: es.clusterID,
		MemberID:  es.memberID,
		Revision:  es.store.currentRevision(),
		RaftTerm:  es.raftTerm,
	}
}

// handlePut은 PUT /v3/kv/put 엔드포인트를 처리한다.
// etcd의 v3rpc/kv.go Put() 메서드 재현
func (es *EtcdServer) handlePut(w http.ResponseWriter, r *http.Request) {
	var req PutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	es.store.put(req.Key, req.Value)

	resp := PutResponse{Header: es.makeHeader()}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleRange는 POST /v3/kv/range 엔드포인트를 처리한다.
// etcd의 v3rpc/kv.go Range() 메서드 재현
func (es *EtcdServer) handleRange(w http.ResponseWriter, r *http.Request) {
	var req RangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var kvs []*KeyValue
	if req.RangeEnd != "" {
		// 프리픽스 범위 조회
		kvs = es.store.rangeGet(req.Key)
	} else {
		// 단일 키 조회
		if kv, ok := es.store.get(req.Key); ok {
			kvs = []*KeyValue{kv}
		}
	}

	resp := RangeResponse{
		Header: es.makeHeader(),
		Kvs:    kvs,
		Count:  int64(len(kvs)),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleDeleteRange는 POST /v3/kv/deleterange 엔드포인트를 처리한다.
func (es *EtcdServer) handleDeleteRange(w http.ResponseWriter, r *http.Request) {
	var req DeleteRangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var deleted int64
	if _, ok := es.store.delete(req.Key); ok {
		deleted = 1
	}

	resp := DeleteRangeResponse{
		Header:  es.makeHeader(),
		Deleted: deleted,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleWatch는 GET /v3/watch 엔드포인트를 처리한다 (SSE 방식).
// etcd의 v3rpc/watch.go의 양방향 스트리밍을 SSE로 시뮬레이션한다.
func (es *EtcdServer) handleWatch(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "스트리밍 미지원", http.StatusInternalServerError)
		return
	}

	watchID, ch := es.store.addWatcher()
	defer es.store.removeWatcher(watchID)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case event, ok := <-ch:
			if !ok {
				return
			}
			resp := WatchResponse{
				Header:  es.makeHeader(),
				Events:  []*WatchEvent{event},
				WatchID: watchID,
			}
			data, _ := json.Marshal(resp)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-ctx.Done():
			return
		}
	}
}

// Start는 HTTP 서버를 시작한다.
func (es *EtcdServer) Start(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/kv/put", es.handlePut)
	mux.HandleFunc("/v3/kv/range", es.handleRange)
	mux.HandleFunc("/v3/kv/deleterange", es.handleDeleteRange)
	mux.HandleFunc("/v3/watch", es.handleWatch)

	es.server = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	go es.server.Serve(listener)
	return nil
}

// Stop은 서버를 종료한다.
func (es *EtcdServer) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	es.server.Shutdown(ctx)
}

// ===== HTTP 클라이언트 헬퍼 =====

func httpPut(baseURL, key, value string) (*PutResponse, error) {
	req := PutRequest{Key: key, Value: value}
	body, _ := json.Marshal(req)
	resp, err := http.Post(baseURL+"/v3/kv/put", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var putResp PutResponse
	json.NewDecoder(resp.Body).Decode(&putResp)
	return &putResp, nil
}

func httpRange(baseURL, key, rangeEnd string) (*RangeResponse, error) {
	req := RangeRequest{Key: key, RangeEnd: rangeEnd}
	body, _ := json.Marshal(req)
	resp, err := http.Post(baseURL+"/v3/kv/range", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var rangeResp RangeResponse
	json.NewDecoder(resp.Body).Decode(&rangeResp)
	return &rangeResp, nil
}

func httpDelete(baseURL, key string) (*DeleteRangeResponse, error) {
	req := DeleteRangeRequest{Key: key}
	body, _ := json.Marshal(req)
	resp, err := http.Post(baseURL+"/v3/kv/deleterange", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var delResp DeleteRangeResponse
	json.NewDecoder(resp.Body).Decode(&delResp)
	return &delResp, nil
}

// ===== 메인 =====

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║  etcd gRPC 서버 PoC (HTTP/JSON 시뮬레이션)             ║")
	fmt.Println("║  KV Put/Range/Delete + Watch(SSE) 서비스               ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")

	// 서버 시작
	server := NewEtcdServer(
		0x1234567890abcdef, // cluster_id
		0xfedcba0987654321, // member_id
		42,                  // raft_term
	)

	addr := "127.0.0.1:12380"
	if err := server.Start(addr); err != nil {
		fmt.Printf("서버 시작 실패: %v\n", err)
		return
	}
	defer server.Stop()

	baseURL := "http://" + addr
	fmt.Printf("\n  서버 시작: %s\n", baseURL)
	time.Sleep(100 * time.Millisecond) // 서버 시작 대기

	// ========================================
	// 1. KV Put
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("1단계: KV Put 요청")
	fmt.Println(strings.Repeat("─", 55))

	putResp, err := httpPut(baseURL, "/app/config", "debug=true")
	if err != nil {
		fmt.Printf("  PUT 실패: %v\n", err)
		return
	}
	fmt.Printf("  PUT /app/config → revision=%d\n", putResp.Header.Revision)
	fmt.Printf("    ResponseHeader: cluster_id=%x, member_id=%x, raft_term=%d\n",
		putResp.Header.ClusterID, putResp.Header.MemberID, putResp.Header.RaftTerm)

	putResp, _ = httpPut(baseURL, "/app/name", "myservice")
	fmt.Printf("  PUT /app/name → revision=%d\n", putResp.Header.Revision)

	putResp, _ = httpPut(baseURL, "/db/host", "localhost:5432")
	fmt.Printf("  PUT /db/host → revision=%d\n", putResp.Header.Revision)

	putResp, _ = httpPut(baseURL, "/app/config", "debug=false")
	fmt.Printf("  PUT /app/config (업데이트) → revision=%d\n", putResp.Header.Revision)

	// ========================================
	// 2. KV Range (단일 키 조회)
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("2단계: KV Range 요청 (단일 키)")
	fmt.Println(strings.Repeat("─", 55))

	rangeResp, err := httpRange(baseURL, "/app/config", "")
	if err != nil {
		fmt.Printf("  RANGE 실패: %v\n", err)
		return
	}
	fmt.Printf("  GET /app/config → count=%d\n", rangeResp.Count)
	for _, kv := range rangeResp.Kvs {
		fmt.Printf("    key=%s value=%q ver=%d create_rev=%d mod_rev=%d\n",
			kv.Key, kv.Value, kv.Version, kv.CreateRevision, kv.ModRevision)
	}

	// ========================================
	// 3. KV Range (프리픽스 조회)
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("3단계: KV Range 요청 (프리픽스)")
	fmt.Println(strings.Repeat("─", 55))

	rangeResp, _ = httpRange(baseURL, "/app/", "/app0")
	fmt.Printf("  GET /app/* → count=%d\n", rangeResp.Count)
	for _, kv := range rangeResp.Kvs {
		fmt.Printf("    key=%s value=%q\n", kv.Key, kv.Value)
	}

	// 존재하지 않는 키
	rangeResp, _ = httpRange(baseURL, "/nonexistent", "")
	fmt.Printf("  GET /nonexistent → count=%d (빈 결과)\n", rangeResp.Count)

	// ========================================
	// 4. Watch (SSE 스트리밍)
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("4단계: Watch (SSE 스트리밍)")
	fmt.Println(strings.Repeat("─", 55))

	// Watch 시작 (별도 고루틴)
	watchEvents := make([]WatchResponse, 0)
	var watchMu sync.Mutex
	watchDone := make(chan struct{})

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		req, _ := http.NewRequestWithContext(ctx, "GET", baseURL+"/v3/watch", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			close(watchDone)
			return
		}
		defer resp.Body.Close()

		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				lines := strings.Split(string(buf[:n]), "\n")
				for _, line := range lines {
					if strings.HasPrefix(line, "data: ") {
						data := strings.TrimPrefix(line, "data: ")
						var wr WatchResponse
						if json.Unmarshal([]byte(data), &wr) == nil {
							watchMu.Lock()
							watchEvents = append(watchEvents, wr)
							watchMu.Unlock()
						}
					}
				}
			}
			if err == io.EOF || err != nil {
				break
			}
		}
		close(watchDone)
	}()

	// Watch가 등록될 때까지 잠시 대기
	time.Sleep(200 * time.Millisecond)

	// Watch 중에 변경 발생
	fmt.Println("  Watch 등록 완료, 변경 이벤트 발생 중...")

	httpPut(baseURL, "/app/config", "debug=true")
	fmt.Println("  → PUT /app/config = debug=true")

	httpPut(baseURL, "/app/version", "v2.0")
	fmt.Println("  → PUT /app/version = v2.0")

	httpDelete(baseURL, "/app/name")
	fmt.Println("  → DELETE /app/name")

	// 이벤트 수신 대기
	time.Sleep(500 * time.Millisecond)

	watchMu.Lock()
	fmt.Printf("\n  수신된 Watch 이벤트: %d개\n", len(watchEvents))
	for i, wr := range watchEvents {
		for _, ev := range wr.Events {
			fmt.Printf("    [%d] watch_id=%d type=%s key=%s",
				i+1, wr.WatchID, ev.Type, ev.Kv.Key)
			if ev.Type == "PUT" {
				fmt.Printf(" value=%q", ev.Kv.Value)
			}
			fmt.Println()
		}
	}
	watchMu.Unlock()

	// ========================================
	// 5. Delete 및 확인
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("5단계: Delete 및 확인")
	fmt.Println(strings.Repeat("─", 55))

	delResp, _ := httpDelete(baseURL, "/db/host")
	fmt.Printf("  DELETE /db/host → deleted=%d, revision=%d\n",
		delResp.Deleted, delResp.Header.Revision)

	// 삭제 후 조회
	rangeResp, _ = httpRange(baseURL, "/db/host", "")
	fmt.Printf("  GET /db/host → count=%d (삭제됨)\n", rangeResp.Count)

	// 존재하지 않는 키 삭제
	delResp, _ = httpDelete(baseURL, "/nonexistent")
	fmt.Printf("  DELETE /nonexistent → deleted=%d\n", delResp.Deleted)

	// ========================================
	// 6. ResponseHeader 구조
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("6단계: ResponseHeader 구조")
	fmt.Println(strings.Repeat("─", 55))

	rangeResp, _ = httpRange(baseURL, "/app/config", "")
	h := rangeResp.Header
	fmt.Println("  모든 etcd 응답에 ResponseHeader가 포함된다:")
	fmt.Printf("    cluster_id: %x (클러스터 식별자)\n", h.ClusterID)
	fmt.Printf("    member_id:  %x (응답한 멤버 식별자)\n", h.MemberID)
	fmt.Printf("    revision:   %d (현재 키 공간 리비전)\n", h.Revision)
	fmt.Printf("    raft_term:  %d (현재 Raft 텀)\n", h.RaftTerm)

	// ========================================
	// 7. API 엔드포인트 요약
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("7단계: API 엔드포인트 요약")
	fmt.Println(strings.Repeat("─", 55))

	fmt.Println("  etcd v3 API (이 PoC에서 시뮬레이션):")
	fmt.Println("  ┌──────────────────────────────────────────────────┐")
	fmt.Println("  │ PUT  /v3/kv/put         → 키-값 저장            │")
	fmt.Println("  │ POST /v3/kv/range       → 키 조회 (단일/범위)   │")
	fmt.Println("  │ POST /v3/kv/deleterange → 키 삭제               │")
	fmt.Println("  │ GET  /v3/watch          → Watch (SSE 스트리밍)  │")
	fmt.Println("  └──────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  curl 테스트 예시:")
	fmt.Printf("    curl -X POST %s/v3/kv/put -d '{\"key\":\"/foo\",\"value\":\"bar\"}'\n", baseURL)
	fmt.Printf("    curl -X POST %s/v3/kv/range -d '{\"key\":\"/foo\"}'\n", baseURL)
	fmt.Printf("    curl -N %s/v3/watch  (SSE 스트리밍)\n", baseURL)

	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("✓ gRPC 서버 PoC 완료")
	fmt.Println("  - KV Put/Range/DeleteRange 엔드포인트 동작 확인")
	fmt.Println("  - Watch SSE 스트리밍으로 실시간 이벤트 수신 확인")
	fmt.Println("  - ResponseHeader 구조 확인")
	fmt.Println("  - 내부 MVCC 스토어 연동 확인")
}
