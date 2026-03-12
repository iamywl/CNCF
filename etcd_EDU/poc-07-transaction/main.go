// poc-07-transaction: 원자적 비교-교환 트랜잭션 시뮬레이션
//
// etcd의 트랜잭션 시스템(server/etcdserver/txn/txn.go)을 기반으로
// Compare → Success/Failure 분기 실행, 분산 잠금 패턴을 시뮬레이션한다.
//
// 참조: server/etcdserver/txn/txn.go   - 트랜잭션 실행
//       api/etcdserverpb/rpc.proto     - Txn RPC 정의
//
// 실행: go run main.go

package main

import (
	"fmt"
	"strings"
	"sync"
)

// ========== 비교 타겟 및 연산자 ==========

// CompareTarget은 비교 대상 필드
// etcd의 pb.Compare_CompareTarget에 해당
type CompareTarget int

const (
	TargetValue      CompareTarget = iota // 값 비교
	TargetVersion                         // 버전 비교
	TargetCreateRev                       // 생성 리비전 비교
	TargetModRev                          // 수정 리비전 비교
)

func (t CompareTarget) String() string {
	switch t {
	case TargetValue:
		return "VALUE"
	case TargetVersion:
		return "VERSION"
	case TargetCreateRev:
		return "CREATE_REV"
	case TargetModRev:
		return "MOD_REV"
	default:
		return "UNKNOWN"
	}
}

// CompareResult는 비교 연산자
// etcd의 pb.Compare_CompareResult에 해당
type CompareResult int

const (
	Equal    CompareResult = iota // ==
	NotEqual                      // !=
	Greater                       // >
	Less                          // <
)

func (r CompareResult) String() string {
	switch r {
	case Equal:
		return "=="
	case NotEqual:
		return "!="
	case Greater:
		return ">"
	case Less:
		return "<"
	default:
		return "?"
	}
}

// ========== 데이터 모델 ==========

// KeyValue는 etcd의 mvccpb.KeyValue에 해당
type KeyValue struct {
	Key            string
	Value          string
	Version        int64 // 키의 버전 (Put마다 증가)
	CreateRevision int64 // 생성 시 리비전
	ModRevision    int64 // 마지막 수정 리비전
}

// ========== 비교 조건 ==========

// Compare는 트랜잭션의 비교 조건
// etcd의 pb.Compare에 해당
type Compare struct {
	Key    string
	Target CompareTarget
	Result CompareResult
	// 비교 값 (타겟에 따라 하나만 사용)
	ValueTarget   string
	IntTarget     int64
}

func (c Compare) String() string {
	switch c.Target {
	case TargetValue:
		return fmt.Sprintf("%s.value %s %q", c.Key, c.Result, c.ValueTarget)
	case TargetVersion:
		return fmt.Sprintf("%s.version %s %d", c.Key, c.Result, c.IntTarget)
	case TargetCreateRev:
		return fmt.Sprintf("%s.create_rev %s %d", c.Key, c.Result, c.IntTarget)
	case TargetModRev:
		return fmt.Sprintf("%s.mod_rev %s %d", c.Key, c.Result, c.IntTarget)
	default:
		return "unknown compare"
	}
}

// ========== 연산 ==========

// OpType은 연산 유형
type OpType int

const (
	OpPut    OpType = iota // 키 저장
	OpGet                  // 키 조회
	OpDelete               // 키 삭제
)

// Op는 트랜잭션 내 연산
// etcd의 pb.RequestOp에 해당
type Op struct {
	Type  OpType
	Key   string
	Value string // Put용
}

func (o Op) String() string {
	switch o.Type {
	case OpPut:
		return fmt.Sprintf("PUT(%s, %q)", o.Key, o.Value)
	case OpGet:
		return fmt.Sprintf("GET(%s)", o.Key)
	case OpDelete:
		return fmt.Sprintf("DELETE(%s)", o.Key)
	default:
		return "UNKNOWN"
	}
}

// ========== 트랜잭션 요청 ==========

// TxnRequest는 트랜잭션 요청
// etcd의 pb.TxnRequest에 해당
//
// 구조: IF compare THEN success ELSE failure
type TxnRequest struct {
	Compare []Compare // IF 조건들 (모두 AND)
	Success []Op      // THEN 실행할 연산들
	Failure []Op      // ELSE 실행할 연산들
}

// TxnResponse는 트랜잭션 응답
// etcd의 pb.TxnResponse에 해당
type TxnResponse struct {
	Succeeded bool            // 조건 만족 여부
	Responses []OpResponse    // 실행된 연산 결과들
	Revision  int64           // 응답 시점의 리비전
}

// OpResponse는 개별 연산 결과
type OpResponse struct {
	Type  OpType
	Key   string
	Value string // Get용
	Found bool   // Get 결과 존재 여부
}

// ========== KV 저장소 (MVCC 단순화) ==========

// MVCCStore는 버전 관리 키-값 저장소
// etcd의 mvcc.store를 단순화
type MVCCStore struct {
	mu       sync.Mutex
	data     map[string]*KeyValue
	revision int64
}

func NewMVCCStore() *MVCCStore {
	return &MVCCStore{
		data:     make(map[string]*KeyValue),
		revision: 1,
	}
}

func (s *MVCCStore) Put(key, value string) {
	s.revision++
	kv, exists := s.data[key]
	if exists {
		kv.Value = value
		kv.Version++
		kv.ModRevision = s.revision
	} else {
		s.data[key] = &KeyValue{
			Key:            key,
			Value:          value,
			Version:        1,
			CreateRevision: s.revision,
			ModRevision:    s.revision,
		}
	}
}

func (s *MVCCStore) Get(key string) (*KeyValue, bool) {
	kv, ok := s.data[key]
	if !ok {
		return nil, false
	}
	// 복사본 반환
	copy := *kv
	return &copy, true
}

func (s *MVCCStore) Delete(key string) bool {
	_, exists := s.data[key]
	if exists {
		s.revision++
		delete(s.data, key)
	}
	return exists
}

func (s *MVCCStore) CurrentRevision() int64 {
	return s.revision
}

// ========== 트랜잭션 실행 엔진 ==========

// TxnEngine은 트랜잭션을 원자적으로 실행한다
// etcd의 txn.Txn() 함수에 해당
type TxnEngine struct {
	store *MVCCStore
}

func NewTxnEngine(store *MVCCStore) *TxnEngine {
	return &TxnEngine{store: store}
}

// Execute는 트랜잭션을 원자적으로 실행한다
// etcd의 txn.Txn() → compareToPath() → executeTxn() 흐름을 재현
func (e *TxnEngine) Execute(req *TxnRequest) *TxnResponse {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()

	resp := &TxnResponse{}

	// 1단계: 조건 평가 (etcd의 applyCompares에 해당)
	resp.Succeeded = e.applyCompares(req.Compare)

	// 2단계: 조건에 따라 Success 또는 Failure 연산 실행
	var ops []Op
	if resp.Succeeded {
		ops = req.Success
	} else {
		ops = req.Failure
	}

	// 3단계: 연산 실행 (etcd의 executeTxn에 해당)
	for _, op := range ops {
		opResp := e.executeOp(op)
		resp.Responses = append(resp.Responses, opResp)
	}

	resp.Revision = e.store.revision
	return resp
}

// applyCompares는 모든 비교 조건을 평가한다
// etcd의 txn.applyCompares()에 해당
func (e *TxnEngine) applyCompares(compares []Compare) bool {
	for _, c := range compares {
		if !e.applyCompare(c) {
			return false
		}
	}
	return true
}

// applyCompare는 단일 비교 조건을 평가한다
// etcd의 txn.compareKV()에 해당
func (e *TxnEngine) applyCompare(c Compare) bool {
	kv, exists := e.store.data[c.Key]

	var result int

	switch c.Target {
	case TargetValue:
		if !exists {
			return false // 값 비교는 키가 없으면 항상 실패
		}
		result = strings.Compare(kv.Value, c.ValueTarget)

	case TargetVersion:
		var ver int64
		if exists {
			ver = kv.Version
		}
		result = compareInt64(ver, c.IntTarget)

	case TargetCreateRev:
		var rev int64
		if exists {
			rev = kv.CreateRevision
		}
		result = compareInt64(rev, c.IntTarget)

	case TargetModRev:
		var rev int64
		if exists {
			rev = kv.ModRevision
		}
		result = compareInt64(rev, c.IntTarget)
	}

	// 연산자 적용 (etcd의 compareKV 결과 판정)
	switch c.Result {
	case Equal:
		return result == 0
	case NotEqual:
		return result != 0
	case Greater:
		return result > 0
	case Less:
		return result < 0
	}
	return false
}

// compareInt64는 두 int64를 비교한다
// etcd의 txn.compareInt64()과 동일
func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// executeOp는 단일 연산을 실행한다
func (e *TxnEngine) executeOp(op Op) OpResponse {
	switch op.Type {
	case OpPut:
		e.store.Put(op.Key, op.Value)
		return OpResponse{Type: OpPut, Key: op.Key}
	case OpGet:
		kv, found := e.store.Get(op.Key)
		resp := OpResponse{Type: OpGet, Key: op.Key, Found: found}
		if found {
			resp.Value = kv.Value
		}
		return resp
	case OpDelete:
		found := e.store.Delete(op.Key)
		return OpResponse{Type: OpDelete, Key: op.Key, Found: found}
	}
	return OpResponse{}
}

// ========== 헬퍼 함수 ==========

func printTxnResult(name string, req *TxnRequest, resp *TxnResponse) {
	fmt.Printf("\n  트랜잭션: %s\n", name)
	fmt.Println("  ┌──────────────────────────────────────")

	// IF 조건 출력
	fmt.Print("  │ IF    ")
	for i, c := range req.Compare {
		if i > 0 {
			fmt.Print("\n  │   AND ")
		}
		fmt.Print(c.String())
	}
	fmt.Println()

	// THEN 출력
	fmt.Print("  │ THEN  ")
	for i, op := range req.Success {
		if i > 0 {
			fmt.Print(", ")
		}
		fmt.Print(op.String())
	}
	fmt.Println()

	// ELSE 출력
	fmt.Print("  │ ELSE  ")
	for i, op := range req.Failure {
		if i > 0 {
			fmt.Print(", ")
		}
		fmt.Print(op.String())
	}
	fmt.Println()

	// 결과
	fmt.Println("  ├──────────────────────────────────────")
	if resp.Succeeded {
		fmt.Println("  │ 결과: SUCCESS (THEN 실행)")
	} else {
		fmt.Println("  │ 결과: FAILURE (ELSE 실행)")
	}
	for _, r := range resp.Responses {
		switch r.Type {
		case OpPut:
			fmt.Printf("  │   → PUT %s 완료\n", r.Key)
		case OpGet:
			if r.Found {
				fmt.Printf("  │   → GET %s = %q\n", r.Key, r.Value)
			} else {
				fmt.Printf("  │   → GET %s = (없음)\n", r.Key)
			}
		case OpDelete:
			fmt.Printf("  │   → DELETE %s (존재=%v)\n", r.Key, r.Found)
		}
	}
	fmt.Printf("  │ 리비전: %d\n", resp.Revision)
	fmt.Println("  └──────────────────────────────────────")
}

func printStore(store *MVCCStore) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.data) == 0 {
		fmt.Println("  (비어 있음)")
		return
	}
	fmt.Println("  ┌──────────────────┬──────────────┬─────────┬────────┬────────┐")
	fmt.Println("  │ 키               │ 값           │ 버전    │ 생성Rev│ 수정Rev│")
	fmt.Println("  ├──────────────────┼──────────────┼─────────┼────────┼────────┤")
	for _, kv := range store.data {
		fmt.Printf("  │ %-16s │ %-12s │   %3d   │  %3d   │  %3d   │\n",
			kv.Key, kv.Value, kv.Version, kv.CreateRevision, kv.ModRevision)
	}
	fmt.Println("  └──────────────────┴──────────────┴─────────┴────────┴────────┘")
}

// ========== 메인 ==========

func main() {
	fmt.Println("==========================================================")
	fmt.Println(" etcd PoC-07: 원자적 비교-교환 트랜잭션")
	fmt.Println("==========================================================")
	fmt.Println()

	store := NewMVCCStore()
	engine := NewTxnEngine(store)

	// 초기 데이터 설정
	store.mu.Lock()
	store.Put("/config/version", "1.0")
	store.Put("/counter", "0")
	store.mu.Unlock()

	fmt.Println("[1] 초기 상태")
	fmt.Println("──────────────────────────────────────")
	printStore(store)

	// 데모 1: CAS (Compare-And-Swap) - 성공 케이스
	fmt.Println("\n[2] CAS (Compare-And-Swap) - 조건 만족")
	fmt.Println("──────────────────────────────────────")

	txn1 := &TxnRequest{
		Compare: []Compare{
			{Key: "/config/version", Target: TargetValue, Result: Equal, ValueTarget: "1.0"},
		},
		Success: []Op{
			{Type: OpPut, Key: "/config/version", Value: "2.0"},
		},
		Failure: []Op{
			{Type: OpGet, Key: "/config/version"},
		},
	}
	resp1 := engine.Execute(txn1)
	printTxnResult("버전 업데이트 (1.0 → 2.0)", txn1, resp1)

	// 데모 2: CAS - 실패 케이스 (이미 2.0으로 변경됨)
	fmt.Println("\n[3] CAS (Compare-And-Swap) - 조건 불만족")
	fmt.Println("──────────────────────────────────────")

	txn2 := &TxnRequest{
		Compare: []Compare{
			{Key: "/config/version", Target: TargetValue, Result: Equal, ValueTarget: "1.0"},
		},
		Success: []Op{
			{Type: OpPut, Key: "/config/version", Value: "3.0"},
		},
		Failure: []Op{
			{Type: OpGet, Key: "/config/version"},
		},
	}
	resp2 := engine.Execute(txn2)
	printTxnResult("버전 업데이트 실패 (1.0이 아님)", txn2, resp2)

	// 데모 3: 분산 잠금 획득 (키가 없으면 생성)
	fmt.Println("\n[4] 분산 잠금 패턴 - 잠금 획득")
	fmt.Println("──────────────────────────────────────")

	lockTxn := &TxnRequest{
		Compare: []Compare{
			// create_revision == 0 → 키가 존재하지 않음
			{Key: "/locks/mylock", Target: TargetCreateRev, Result: Equal, IntTarget: 0},
		},
		Success: []Op{
			{Type: OpPut, Key: "/locks/mylock", Value: "owner-A"},
		},
		Failure: []Op{
			{Type: OpGet, Key: "/locks/mylock"},
		},
	}
	lockResp := engine.Execute(lockTxn)
	printTxnResult("잠금 획득 (owner-A)", lockTxn, lockResp)

	// 데모 4: 분산 잠금 - 이미 잠겨 있음
	fmt.Println("\n[5] 분산 잠금 패턴 - 잠금 충돌")
	fmt.Println("──────────────────────────────────────")

	lockTxn2 := &TxnRequest{
		Compare: []Compare{
			{Key: "/locks/mylock", Target: TargetCreateRev, Result: Equal, IntTarget: 0},
		},
		Success: []Op{
			{Type: OpPut, Key: "/locks/mylock", Value: "owner-B"},
		},
		Failure: []Op{
			{Type: OpGet, Key: "/locks/mylock"},
		},
	}
	lockResp2 := engine.Execute(lockTxn2)
	printTxnResult("잠금 획득 실패 (owner-B)", lockTxn2, lockResp2)

	// 데모 5: 다중 조건 + 다중 연산
	fmt.Println("\n[6] 다중 조건 트랜잭션")
	fmt.Println("──────────────────────────────────────")

	multiTxn := &TxnRequest{
		Compare: []Compare{
			{Key: "/config/version", Target: TargetValue, Result: Equal, ValueTarget: "2.0"},
			{Key: "/config/version", Target: TargetVersion, Result: Greater, IntTarget: 0},
		},
		Success: []Op{
			{Type: OpPut, Key: "/config/version", Value: "3.0"},
			{Type: OpPut, Key: "/config/updated_by", Value: "admin"},
			{Type: OpDelete, Key: "/locks/mylock"},
		},
		Failure: []Op{
			{Type: OpGet, Key: "/config/version"},
		},
	}
	multiResp := engine.Execute(multiTxn)
	printTxnResult("다중 조건 업데이트", multiTxn, multiResp)

	// 데모 6: 동시성 테스트 - 카운터 증가
	fmt.Println("\n[7] 동시성: CAS 기반 카운터 증가")
	fmt.Println("──────────────────────────────────────")

	var wg sync.WaitGroup
	successCount := 0
	failCount := 0
	var countMu sync.Mutex

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			// CAS 재시도 루프
			for retry := 0; retry < 10; retry++ {
				// 현재 값 읽기
				store.mu.Lock()
				kv, _ := store.Get("/counter")
				currentVal := "0"
				if kv != nil {
					currentVal = kv.Value
				}
				store.mu.Unlock()

				// CAS로 증가 시도
				newVal := fmt.Sprintf("%d", atoi(currentVal)+1)
				txn := &TxnRequest{
					Compare: []Compare{
						{Key: "/counter", Target: TargetValue, Result: Equal, ValueTarget: currentVal},
					},
					Success: []Op{
						{Type: OpPut, Key: "/counter", Value: newVal},
					},
					Failure: []Op{},
				}
				resp := engine.Execute(txn)
				if resp.Succeeded {
					countMu.Lock()
					successCount++
					countMu.Unlock()
					fmt.Printf("  Worker-%d: %s → %s (성공)\n", workerID, currentVal, newVal)
					return
				}
				countMu.Lock()
				failCount++
				countMu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	fmt.Printf("\n  CAS 카운터 결과:\n")
	fmt.Printf("    성공: %d회, 재시도(충돌): %d회\n", successCount, failCount)

	// 최종 상태
	fmt.Println("\n[8] 최종 상태")
	fmt.Println("──────────────────────────────────────")
	printStore(store)

	// 요약
	fmt.Println("\n==========================================================")
	fmt.Println(" 시뮬레이션 요약")
	fmt.Println("==========================================================")
	fmt.Println()
	fmt.Println("  etcd 트랜잭션 시스템의 핵심 동작:")
	fmt.Println("  1. Compare: 키의 value/version/create_rev/mod_rev 조건 검사")
	fmt.Println("  2. IF-THEN-ELSE: 조건 만족 시 Success, 불만족 시 Failure 실행")
	fmt.Println("  3. 원자적 실행: 락 기반으로 비교와 실행이 원자적으로 수행")
	fmt.Println("  4. 분산 잠금: create_rev == 0 조건으로 키 미존재 확인")
	fmt.Println("  5. CAS 패턴: 읽기 → 비교 → 교환으로 동시성 안전한 업데이트")
	fmt.Println()
	fmt.Println("  참조 소스:")
	fmt.Println("  - server/etcdserver/txn/txn.go      (트랜잭션 실행)")
	fmt.Println("  - api/etcdserverpb/rpc.proto         (Txn RPC 정의)")
	fmt.Println()
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}
