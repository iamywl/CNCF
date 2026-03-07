package main

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Kafka Transaction 2PC Protocol Simulation
// Based on: TransactionCoordinator.scala, TransactionState.java
//
// Kafka의 트랜잭션은 2-Phase Commit (2PC) 프로토콜을 사용하여
// 여러 파티션에 걸친 원자적 쓰기를 보장한다.
//
// 상태 전이 (TransactionState.java):
//   Empty -> Ongoing -> PrepareCommit -> CompleteCommit
//                    -> PrepareAbort  -> CompleteAbort
//
// 핵심 흐름:
//   1. InitProducerId: PID + epoch 할당
//   2. AddPartitionsToTxn: 트랜잭션에 파티션 등록
//   3. Produce: 트랜잭션 플래그와 함께 데이터 전송
//   4. EndTxn(COMMIT/ABORT): 2PC 프로토콜로 커밋/어보트
//   5. WriteTxnMarkers: 각 파티션에 COMMIT/ABORT 마커 기록
// =============================================================================

// TransactionState는 트랜잭션 상태를 나타낸다.
// TransactionState.java 열거형에 대응한다.
type TransactionState int

const (
	TxnEmpty           TransactionState = iota // 초기 상태
	TxnOngoing                                 // 트랜잭션 진행 중
	TxnPrepareCommit                           // Phase 1: 커밋 준비
	TxnPrepareAbort                            // Phase 1: 어보트 준비
	TxnCompleteCommit                          // Phase 2: 커밋 완료
	TxnCompleteAbort                           // Phase 2: 어보트 완료
	TxnDead                                    // 만료됨
)

// validPreviousStates는 각 상태의 유효한 이전 상태를 정의한다.
// TransactionState.VALID_PREVIOUS_STATES에 대응한다.
var validPreviousStates = map[TransactionState][]TransactionState{
	TxnEmpty:         {TxnEmpty, TxnCompleteCommit, TxnCompleteAbort},
	TxnOngoing:       {TxnOngoing, TxnEmpty, TxnCompleteCommit, TxnCompleteAbort},
	TxnPrepareCommit: {TxnOngoing},
	TxnPrepareAbort:  {TxnOngoing},
	TxnCompleteCommit: {TxnPrepareCommit},
	TxnCompleteAbort:  {TxnPrepareAbort},
	TxnDead:           {TxnEmpty, TxnCompleteAbort, TxnCompleteCommit},
}

func (s TransactionState) String() string {
	names := []string{"Empty", "Ongoing", "PrepareCommit", "PrepareAbort",
		"CompleteCommit", "CompleteAbort", "Dead"}
	if int(s) < len(names) {
		return names[s]
	}
	return "Unknown"
}

// TopicPartition은 토픽-파티션을 나타낸다.
type TopicPartition struct {
	Topic     string
	Partition int
}

func (tp TopicPartition) String() string {
	return fmt.Sprintf("%s-%d", tp.Topic, tp.Partition)
}

// TransactionRecord는 트랜잭션 내에서 전송된 레코드를 나타낸다.
type TransactionRecord struct {
	TP        TopicPartition
	Key       string
	Value     string
	PID       int64
	Epoch     int16
	InTxn     bool   // 트랜잭션 플래그
	Committed *bool  // nil=미결정, true=커밋, false=어보트
}

// TransactionMarker는 파티션에 기록되는 트랜잭션 마커이다.
type TransactionMarker struct {
	TP     TopicPartition
	PID    int64
	Epoch  int16
	Type   string // "COMMIT" or "ABORT"
}

// TransactionMetadata는 트랜잭션 코디네이터가 관리하는 트랜잭션 메타데이터이다.
// TransactionMetadata.scala에 대응한다.
type TransactionMetadata struct {
	mu              sync.Mutex
	TransactionalID string
	ProducerID      int64
	ProducerEpoch   int16
	State           TransactionState
	Partitions      map[TopicPartition]bool
	TxnTimeoutMs    int64
	TxnStartTime    time.Time
	PendingState    *TransactionState
}

// TransactionCoordinator는 트랜잭션 코디네이터를 시뮬레이션한다.
// TransactionCoordinator.scala에 대응한다.
type TransactionCoordinator struct {
	mu            sync.Mutex
	transactions  map[string]*TransactionMetadata // transactionalId -> metadata
	nextPID       int64
	partitions    map[TopicPartition][]TransactionRecord // 파티션별 레코드 저장소
	markers       []TransactionMarker
	txnLog        []string // 트랜잭션 로그 (__transaction_state)
}

// NewTransactionCoordinator는 새 트랜잭션 코디네이터를 생성한다.
func NewTransactionCoordinator() *TransactionCoordinator {
	return &TransactionCoordinator{
		transactions: make(map[string]*TransactionMetadata),
		nextPID:      1000,
		partitions:   make(map[TopicPartition][]TransactionRecord),
	}
}

// InitProducerID는 트랜잭션 프로듀서에게 PID와 epoch을 할당한다.
// TransactionCoordinator.handleInitProducerId()에 대응한다.
func (tc *TransactionCoordinator) InitProducerID(transactionalID string) (int64, int16, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	existing, ok := tc.transactions[transactionalID]
	if ok {
		// 기존 트랜잭션이 있으면 epoch 증가 (펜싱)
		existing.mu.Lock()
		defer existing.mu.Unlock()

		if existing.State == TxnOngoing {
			return 0, 0, fmt.Errorf("CONCURRENT_TRANSACTIONS: 진행 중인 트랜잭션 있음")
		}

		existing.ProducerEpoch++
		existing.State = TxnEmpty
		existing.Partitions = make(map[TopicPartition]bool)

		tc.logTxnState(transactionalID, existing)
		return existing.ProducerID, existing.ProducerEpoch, nil
	}

	// 새 프로듀서 ID 할당
	pid := atomic.AddInt64(&tc.nextPID, 1)
	meta := &TransactionMetadata{
		TransactionalID: transactionalID,
		ProducerID:      pid,
		ProducerEpoch:   0,
		State:           TxnEmpty,
		Partitions:      make(map[TopicPartition]bool),
		TxnTimeoutMs:    60000,
	}
	tc.transactions[transactionalID] = meta
	tc.logTxnState(transactionalID, meta)

	return pid, 0, nil
}

// BeginTransaction은 트랜잭션을 시작한다 (클라이언트 측 상태 변경).
func (tc *TransactionCoordinator) BeginTransaction(transactionalID string) error {
	tc.mu.Lock()
	meta, ok := tc.transactions[transactionalID]
	tc.mu.Unlock()

	if !ok {
		return fmt.Errorf("INVALID_PRODUCER_ID_MAPPING")
	}

	meta.mu.Lock()
	defer meta.mu.Unlock()

	if meta.State != TxnEmpty && meta.State != TxnCompleteCommit && meta.State != TxnCompleteAbort {
		return fmt.Errorf("INVALID_TXN_STATE: current=%s", meta.State)
	}

	meta.TxnStartTime = time.Now()
	return nil
}

// AddPartitionsToTxn은 트랜잭션에 파티션을 등록한다.
// Empty/CompleteCommit/CompleteAbort -> Ongoing 전이가 발생한다.
func (tc *TransactionCoordinator) AddPartitionsToTxn(transactionalID string, partitions []TopicPartition) error {
	tc.mu.Lock()
	meta, ok := tc.transactions[transactionalID]
	tc.mu.Unlock()

	if !ok {
		return fmt.Errorf("INVALID_PRODUCER_ID_MAPPING")
	}

	meta.mu.Lock()
	defer meta.mu.Unlock()

	// 상태 전이: Empty/CompleteCommit/CompleteAbort -> Ongoing
	if meta.State != TxnOngoing && meta.State != TxnEmpty &&
		meta.State != TxnCompleteCommit && meta.State != TxnCompleteAbort {
		return fmt.Errorf("INVALID_TXN_STATE: current=%s", meta.State)
	}

	meta.State = TxnOngoing
	for _, tp := range partitions {
		meta.Partitions[tp] = true
	}

	tc.mu.Lock()
	tc.logTxnState(transactionalID, meta)
	tc.mu.Unlock()

	return nil
}

// ProduceToTxn은 트랜잭션 내에서 레코드를 생성한다.
func (tc *TransactionCoordinator) ProduceToTxn(transactionalID string, tp TopicPartition, key, value string) error {
	tc.mu.Lock()
	meta, ok := tc.transactions[transactionalID]
	tc.mu.Unlock()

	if !ok {
		return fmt.Errorf("INVALID_PRODUCER_ID_MAPPING")
	}

	meta.mu.Lock()
	pid := meta.ProducerID
	epoch := meta.ProducerEpoch
	state := meta.State
	meta.mu.Unlock()

	if state != TxnOngoing {
		return fmt.Errorf("INVALID_TXN_STATE: expected=Ongoing, current=%s", state)
	}

	record := TransactionRecord{
		TP:    tp,
		Key:   key,
		Value: value,
		PID:   pid,
		Epoch: epoch,
		InTxn: true,
	}

	tc.mu.Lock()
	tc.partitions[tp] = append(tc.partitions[tp], record)
	tc.mu.Unlock()

	return nil
}

// EndTransaction은 2PC 프로토콜로 트랜잭션을 커밋 또는 어보트한다.
// TransactionCoordinator.handleEndTransaction()에 대응한다.
func (tc *TransactionCoordinator) EndTransaction(transactionalID string, commit bool) error {
	tc.mu.Lock()
	meta, ok := tc.transactions[transactionalID]
	tc.mu.Unlock()

	if !ok {
		return fmt.Errorf("INVALID_PRODUCER_ID_MAPPING")
	}

	meta.mu.Lock()

	if meta.State != TxnOngoing {
		meta.mu.Unlock()
		return fmt.Errorf("INVALID_TXN_STATE: expected=Ongoing, current=%s", meta.State)
	}

	// Phase 1: PrepareCommit/PrepareAbort로 전이
	// __transaction_state 토픽에 기록
	if commit {
		meta.State = TxnPrepareCommit
	} else {
		meta.State = TxnPrepareAbort
	}

	partitions := make([]TopicPartition, 0, len(meta.Partitions))
	for tp := range meta.Partitions {
		partitions = append(partitions, tp)
	}
	pid := meta.ProducerID
	epoch := meta.ProducerEpoch

	tc.mu.Lock()
	tc.logTxnState(transactionalID, meta)
	tc.mu.Unlock()

	meta.mu.Unlock()

	fmt.Printf("    [Phase 1] %s 상태를 __transaction_state에 기록\n", meta.State)

	// Phase 2: 각 파티션에 트랜잭션 마커 기록
	markerType := "ABORT"
	if commit {
		markerType = "COMMIT"
	}

	tc.mu.Lock()
	for _, tp := range partitions {
		marker := TransactionMarker{
			TP:    tp,
			PID:   pid,
			Epoch: epoch,
			Type:  markerType,
		}
		tc.markers = append(tc.markers, marker)

		// 파티션의 레코드에 커밋/어보트 결과 반영
		records := tc.partitions[tp]
		for i := range records {
			if records[i].PID == pid && records[i].Epoch == epoch && records[i].Committed == nil {
				committed := commit
				records[i].Committed = &committed
			}
		}

		fmt.Printf("    [Phase 2] %s 마커를 %s에 기록\n", markerType, tp)
	}
	tc.mu.Unlock()

	// 최종 상태 전이: CompleteCommit/CompleteAbort
	meta.mu.Lock()
	if commit {
		meta.State = TxnCompleteCommit
	} else {
		meta.State = TxnCompleteAbort
	}
	meta.Partitions = make(map[TopicPartition]bool)
	meta.mu.Unlock()

	tc.mu.Lock()
	tc.logTxnState(transactionalID, meta)
	tc.mu.Unlock()

	fmt.Printf("    [완료] 상태: %s\n", meta.State)

	return nil
}

// ReadCommitted는 read_committed 격리 수준으로 레코드를 읽는다.
// 커밋된 트랜잭션의 레코드와 비트랜잭션 레코드만 반환한다.
func (tc *TransactionCoordinator) ReadCommitted(tp TopicPartition) []TransactionRecord {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	var result []TransactionRecord
	for _, record := range tc.partitions[tp] {
		if !record.InTxn {
			// 비트랜잭션 레코드는 항상 포함
			result = append(result, record)
		} else if record.Committed != nil && *record.Committed {
			// 커밋된 트랜잭션 레코드만 포함
			result = append(result, record)
		}
		// 어보트된 레코드와 미결정 레코드는 제외
	}
	return result
}

// ReadUncommitted는 read_uncommitted 격리 수준으로 모든 레코드를 읽는다.
func (tc *TransactionCoordinator) ReadUncommitted(tp TopicPartition) []TransactionRecord {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	result := make([]TransactionRecord, len(tc.partitions[tp]))
	copy(result, tc.partitions[tp])
	return result
}

func (tc *TransactionCoordinator) logTxnState(txnID string, meta *TransactionMetadata) {
	entry := fmt.Sprintf("txnId=%s, pid=%d, epoch=%d, state=%s, partitions=%v",
		txnID, meta.ProducerID, meta.ProducerEpoch, meta.State, partitionKeys(meta.Partitions))
	tc.txnLog = append(tc.txnLog, entry)
}

func partitionKeys(m map[TopicPartition]bool) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k.String())
	}
	return keys
}

func main() {
	fmt.Println("=============================================================")
	fmt.Println("  Kafka Transaction 2PC Protocol Simulation")
	fmt.Println("  Based on: TransactionCoordinator.scala, TransactionState.java")
	fmt.Println("=============================================================")

	tc := NewTransactionCoordinator()

	// =========================================================================
	// 시나리오 1: 정상 트랜잭션 커밋
	// =========================================================================
	fmt.Println("\n--- 시나리오 1: 정상 트랜잭션 커밋 ---")
	fmt.Println("여러 파티션에 원자적으로 쓰기 (커밋)\n")

	// 1단계: InitProducerId
	fmt.Println("  [1] InitProducerId")
	pid1, epoch1, err := tc.InitProducerID("txn-producer-1")
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
		return
	}
	fmt.Printf("    할당: PID=%d, Epoch=%d\n", pid1, epoch1)

	// 2단계: BeginTransaction (클라이언트 측)
	fmt.Println("  [2] BeginTransaction")
	tc.BeginTransaction("txn-producer-1")
	fmt.Println("    트랜잭션 시작")

	// 3단계: AddPartitionsToTxn
	fmt.Println("  [3] AddPartitionsToTxn")
	tp1 := TopicPartition{"orders", 0}
	tp2 := TopicPartition{"inventory", 0}
	tp3 := TopicPartition{"payments", 0}
	tc.AddPartitionsToTxn("txn-producer-1", []TopicPartition{tp1, tp2, tp3})
	fmt.Printf("    등록된 파티션: %s, %s, %s\n", tp1, tp2, tp3)

	// 4단계: Produce (트랜잭션 내)
	fmt.Println("  [4] Produce (트랜잭션 내)")
	tc.ProduceToTxn("txn-producer-1", tp1, "order-001", "item=laptop,qty=1")
	tc.ProduceToTxn("txn-producer-1", tp2, "laptop", "stock=-1")
	tc.ProduceToTxn("txn-producer-1", tp3, "pay-001", "amount=1500")
	fmt.Println("    orders-0: order-001 -> item=laptop,qty=1")
	fmt.Println("    inventory-0: laptop -> stock=-1")
	fmt.Println("    payments-0: pay-001 -> amount=1500")

	// 5단계: EndTxn (COMMIT)
	fmt.Println("  [5] EndTxn(COMMIT) - 2PC 프로토콜")
	tc.EndTransaction("txn-producer-1", true)

	// =========================================================================
	// 시나리오 2: 트랜잭션 어보트
	// =========================================================================
	fmt.Println("\n--- 시나리오 2: 트랜잭션 어보트 ---")
	fmt.Println("오류 발생 시 트랜잭션을 어보트하여 모든 쓰기를 취소\n")

	// 동일 프로듀서로 새 트랜잭션 시작 (epoch이 유지됨)
	fmt.Println("  [1] InitProducerId (epoch bump)")
	pid2, epoch2, _ := tc.InitProducerID("txn-producer-1")
	fmt.Printf("    PID=%d, Epoch=%d (epoch 증가로 이전 프로듀서 펜싱)\n", pid2, epoch2)

	tc.BeginTransaction("txn-producer-1")
	tp4 := TopicPartition{"orders", 1}
	tp5 := TopicPartition{"inventory", 1}
	tc.AddPartitionsToTxn("txn-producer-1", []TopicPartition{tp4, tp5})

	tc.ProduceToTxn("txn-producer-1", tp4, "order-002", "item=phone,qty=2")
	tc.ProduceToTxn("txn-producer-1", tp5, "phone", "stock=-2")
	fmt.Println("  [2] Produce")
	fmt.Println("    orders-1: order-002 -> item=phone,qty=2")
	fmt.Println("    inventory-1: phone -> stock=-2")

	fmt.Println("  [3] EndTxn(ABORT) - 오류 감지, 트랜잭션 어보트")
	tc.EndTransaction("txn-producer-1", false)

	// =========================================================================
	// 시나리오 3: read_committed vs read_uncommitted
	// =========================================================================
	fmt.Println("\n--- 시나리오 3: read_committed vs read_uncommitted 격리 수준 ---")

	fmt.Println("\n  orders-0 파티션 (커밋된 트랜잭션):")
	fmt.Println("  [read_uncommitted] 모든 레코드 반환:")
	allRecords := tc.ReadUncommitted(tp1)
	for _, r := range allRecords {
		status := "미결정"
		if r.Committed != nil {
			if *r.Committed {
				status = "커밋됨"
			} else {
				status = "어보트됨"
			}
		}
		fmt.Printf("    key=%s value=%s [%s]\n", r.Key, r.Value, status)
	}

	fmt.Println("  [read_committed] 커밋된 레코드만 반환:")
	committedRecords := tc.ReadCommitted(tp1)
	for _, r := range committedRecords {
		fmt.Printf("    key=%s value=%s\n", r.Key, r.Value)
	}

	fmt.Println("\n  orders-1 파티션 (어보트된 트랜잭션):")
	fmt.Println("  [read_uncommitted] 모든 레코드 반환:")
	allRecords2 := tc.ReadUncommitted(tp4)
	for _, r := range allRecords2 {
		status := "미결정"
		if r.Committed != nil {
			if *r.Committed {
				status = "커밋됨"
			} else {
				status = "어보트됨"
			}
		}
		fmt.Printf("    key=%s value=%s [%s]\n", r.Key, r.Value, status)
	}

	fmt.Println("  [read_committed] 커밋된 레코드만 반환:")
	committedRecords2 := tc.ReadCommitted(tp4)
	if len(committedRecords2) == 0 {
		fmt.Println("    (없음 - 모든 레코드가 어보트됨)")
	}

	// =========================================================================
	// 시나리오 4: 상태 전이 다이어그램
	// =========================================================================
	fmt.Println("\n--- 시나리오 4: 트랜잭션 상태 전이 ---")
	fmt.Println("\n  유효한 상태 전이 (TransactionState.VALID_PREVIOUS_STATES):")
	fmt.Println("  " + strings.Repeat("-", 60))

	stateNames := []TransactionState{TxnEmpty, TxnOngoing, TxnPrepareCommit, TxnPrepareAbort,
		TxnCompleteCommit, TxnCompleteAbort, TxnDead}

	for _, state := range stateNames {
		prevStates := validPreviousStates[state]
		var prevNames []string
		for _, ps := range prevStates {
			prevNames = append(prevNames, ps.String())
		}
		fmt.Printf("  %-15s <- {%s}\n", state, strings.Join(prevNames, ", "))
	}

	fmt.Println("\n  커밋 흐름: Empty -> Ongoing -> PrepareCommit -> CompleteCommit")
	fmt.Println("  어보트 흐름: Empty -> Ongoing -> PrepareAbort -> CompleteAbort")

	// =========================================================================
	// 시나리오 5: __transaction_state 로그 내용
	// =========================================================================
	fmt.Println("\n--- 시나리오 5: __transaction_state 토픽 로그 ---")
	fmt.Println("트랜잭션 코디네이터의 모든 상태 변경이 기록됨\n")

	tc.mu.Lock()
	for i, entry := range tc.txnLog {
		fmt.Printf("  [%2d] %s\n", i, entry)
	}
	tc.mu.Unlock()

	// =========================================================================
	// 시나리오 6: 프로듀서 펜싱 (Epoch 기반)
	// =========================================================================
	fmt.Println("\n--- 시나리오 6: 프로듀서 펜싱 ---")
	fmt.Println("같은 transactionalId로 InitProducerId를 호출하면 epoch이 증가하여")
	fmt.Println("이전 프로듀서 인스턴스가 자동으로 펜싱(차단)됨\n")

	_, e0, _ := tc.InitProducerID("txn-fence-demo")
	fmt.Printf("  1차 InitProducerId: epoch=%d\n", e0)
	_, e1, _ := tc.InitProducerID("txn-fence-demo")
	fmt.Printf("  2차 InitProducerId: epoch=%d (이전 epoch=%d는 펜싱됨)\n", e1, e0)
	_, e2, _ := tc.InitProducerID("txn-fence-demo")
	fmt.Printf("  3차 InitProducerId: epoch=%d (이전 epoch=%d는 펜싱됨)\n", e2, e1)

	fmt.Println("\n  epoch가 증가할 때마다 이전 epoch을 가진 프로듀서의 요청은")
	fmt.Println("  PRODUCER_FENCED 에러로 거부되어 좀비 프로듀서를 방지한다.")

	// =========================================================================
	// 핵심 알고리즘 요약
	// =========================================================================
	fmt.Println("\n=============================================================")
	fmt.Println("  핵심 알고리즘 요약")
	fmt.Println("=============================================================")
	fmt.Println(`
  Kafka Transaction 2PC 프로토콜:

  1. InitProducerId (TransactionCoordinator.handleInitProducerId):
     - transactionalId -> PID + epoch 할당
     - 동일 transactionalId 재사용 시 epoch 증가 (프로듀서 펜싱)
     - __transaction_state 토픽에 기록

  2. AddPartitionsToTxn:
     - 트랜잭션에 참여할 파티션 등록
     - 상태: Empty/Complete* -> Ongoing

  3. Produce (트랜잭션 내):
     - 각 레코드에 PID, epoch, 트랜잭션 플래그 포함
     - 데이터는 파티션에 기록되지만 아직 "미결정" 상태

  4. EndTxn - 2PC 프로토콜 (TransactionCoordinator.endTransaction):
     [Phase 1] PrepareCommit/PrepareAbort
       - __transaction_state 토픽에 상태 기록
       - 이 기록이 성공하면 결과가 결정됨 (crash-safe)

     [Phase 2] WriteTxnMarkers
       - 각 참여 파티션에 COMMIT/ABORT 마커 기록
       - 마커 기록 완료 후 CompleteCommit/CompleteAbort로 전이

  5. read_committed 격리 수준:
     - Last Stable Offset (LSO) 이하의 레코드만 반환
     - 어보트된 트랜잭션의 레코드는 .aborted_txns 인덱스로 필터링
     - 미결정 트랜잭션의 레코드는 반환하지 않음

  6. 프로듀서 펜싱:
     - 동일 transactionalId의 epoch이 증가하면 이전 프로듀서 차단
     - 좀비 프로듀서(네트워크 파티션 후 복귀한 구 인스턴스) 방지
`)
}
