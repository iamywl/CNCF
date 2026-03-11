# Alertmanager Notification Log (nflog) Deep Dive

## 1. 개요

Notification Log(nflog)는 알림 발송 기록을 저장하여 **중복 알림 방지**와 **RepeatInterval 제어**를 담당한다. `nflog/nflog.go`에 구현되어 있으며, 클러스터 간 Gossip으로 동기화된다.

## 2. Log 구조체

```go
// nflog/nflog.go
type Log struct {
    clock      quartz.Clock
    logger     *slog.Logger
    metrics    *metrics
    retention  time.Duration     // 엔트리 보존 기간

    mtx        sync.RWMutex
    st         state             // 핵심 상태
    broadcast  func([]byte)      // 클러스터 브로드캐스트
}
```

### 2.1 state 타입

```go
type state map[string]*pb.MeshEntry
```

키 형식: `{groupKey}:{receiverGroupName}:{integrationName}:{integrationIdx}`

### 2.2 MeshEntry (Protobuf)

```
MeshEntry:
    Entry entry:
        string group_key           // 그룹 키
        Receiver receiver          // 수신자 정보
        []uint64 firing_alerts     // firing Alert Fingerprint 해시
        []uint64 resolved_alerts   // resolved Alert Fingerprint 해시
        Timestamp timestamp        // 발송 시간
        map receiver_data          // 커스텀 데이터 (Store)
    Timestamp expires_at           // 만료 시간
```

## 3. Store (key-value)

```go
// nflog/nflog.go
type Store struct {
    data map[string]*pb.ReceiverDataValue
}

func NewStore(entry *pb.Entry) *Store
func (s *Store) GetInt(key string) (int64, bool)
func (s *Store) GetFloat(key string) (float64, bool)
func (s *Store) GetStr(key string) (string, bool)
func (s *Store) SetInt(key string, v int64)
func (s *Store) SetFloat(key string, v float64)
func (s *Store) SetStr(key, v string)
func (s *Store) Delete(key string)
```

각 nflog 엔트리에 부가 데이터를 저장할 수 있다. Pipeline의 Stage들이 커스텀 데이터를 저장/조회하는 데 사용한다.

## 4. Log() — 발송 기록 저장

```
nflog.Log(ctx, entries...*pb.Entry):
    각 entry에 대해:
    1. 키 생성: groupKey + receiver 정보
    2. MeshEntry 생성:
       - entry: 발송 기록
       - expires_at: now + retention
    3. st[key] = meshEntry
    4. broadcast(직렬화된 meshEntry)
       → 클러스터 다른 인스턴스에 전파
```

## 5. Query() — 발송 기록 조회

```go
// nflog/nflog.go
type QueryParam func(*query) error

func QReceiver(r *pb.Receiver) QueryParam
func QGroupKey(gk string) QueryParam
```

```
nflog.Query(ctx, params...):
    1. 쿼리 파라미터 적용
    2. st를 순회하며 조건에 맞는 엔트리 수집
    3. 만료되지 않은 엔트리만 반환
    4. []*pb.Entry 반환
```

## 6. DedupStage와의 연동

DedupStage는 nflog을 참조하여 중복 알림을 방지한다:

```
DedupStage.Exec(ctx, alerts...):
    1. nflog.Query(receiver, groupKey)
       → 이전 발송 기록 조회

    2. 이전 기록이 없으면:
       → 모든 Alert 통과 (첫 발송)

    3. 이전 기록이 있으면:
       a. 이전 firing_alerts와 현재 firing alerts 비교
       b. 이전 resolved_alerts와 현재 resolved alerts 비교

       변경 감지:
       - 새로 firing된 Alert 있음 → 통과
       - 새로 resolved된 Alert 있음 → 통과
       - 이전과 동일함:
         - RepeatInterval 경과 → 통과 (반복 전송)
         - RepeatInterval 미경과 → 필터링 (중복)

    4. 통과한 Alert만 반환
```

```
시간축:

T0: Alert 발생, 첫 flush
    → DedupStage: nflog 없음 → 통과
    → RetryStage: 알림 전송
    → SetNotifiesStage: nflog 기록

T0+5m: GroupInterval flush (새 Alert 없음)
    → DedupStage: nflog 있음, 동일한 상태
    → RepeatInterval(4h) 미경과 → 필터링

T0+10m: 새 Alert 추가, GroupInterval flush
    → DedupStage: firing_alerts 변경됨 → 통과
    → 알림 전송 + nflog 업데이트

T0+4h: RepeatInterval 경과
    → DedupStage: 동일 상태이지만 4h 경과 → 통과
    → 반복 알림 전송
```

## 7. GC (Garbage Collection)

```go
// nflog/nflog.go
func (l *Log) GC() ([]string, error) {
    // expires_at이 지난 엔트리 삭제
    // 삭제된 키 목록 반환
}
```

```
GC() 흐름:
    now := time.Now()
    for key, entry := range st:
        if entry.ExpiresAt.Before(now):
            delete(st, key)
    return deletedKeys
```

## 8. Maintenance()

```go
// nflog/nflog.go
func (l *Log) Maintenance(interval time.Duration, snapf string, stopc <-chan struct{}, override MaintenanceFunc)
```

```
Maintenance() goroutine:
    ticker := interval

    for {
        select {
        case <-ticker:
            if override != nil:
                override()  // 커스텀 유지보수
            else:
                1. GC()         // 만료 엔트리 삭제
                2. Snapshot(f)  // 디스크 스냅샷 저장
        case <-stopc:
            return
        }
    }
```

## 9. 스냅샷

### 9.1 Snapshot()

```go
func (l *Log) Snapshot(w io.Writer) (int64, error) {
    // st의 모든 MeshEntry를 Protobuf로 직렬화하여 w에 기록
}
```

### 9.2 loadSnapshot()

```go
func (l *Log) loadSnapshot(r io.Reader) error {
    // r에서 Protobuf 데이터 읽기
    // 각 MeshEntry를 st에 복원
}
```

스냅샷 파일 경로: `{storage.path}/nflog`

## 10. 클러스터 동기화

### 10.1 State 인터페이스 구현

```go
// nflog/nflog.go
func (l *Log) MarshalBinary() ([]byte, error) {
    // 전체 st를 Protobuf 직렬화
    // Push-Pull 교환에 사용
}

func (l *Log) Merge(b []byte) error {
    // 수신 데이터 디시리얼라이즈
    // 각 엔트리에 대해 state.merge() 호출
}
```

### 10.2 state.merge()

```go
// nflog/nflog.go
func (s state) merge(e *pb.MeshEntry, now time.Time) bool {
    // 키로 기존 엔트리 조회
    // 없으면: 새로 추가
    // 있으면: timestamp 비교
    //   수신이 더 최신 → 업데이트
    //   기존이 더 최신 → 무시
    // 반환: true=변경됨, false=무시됨
}
```

**최신 타임스탬프 승리** 규칙으로 CRDT 방식의 병합을 수행한다.

### 10.3 동기화 시나리오

```
AM-1에서 Alert 전송:
    1. nflog.Log(entry)  → st에 기록
    2. broadcast(entry)  → Gossip 전파

AM-2에서 수신:
    3. delegate.NotifyMsg(msg)
    4. nflog.Merge(msg)
    5. state.merge(entry, now)
       → st에 기록 (timestamp 확인)

AM-2에서 동일 Alert flush:
    6. DedupStage → nflog.Query()
    7. 이미 AM-1에서 전송 기록 있음
    8. → 중복 필터링 (전송하지 않음)

결과: 클러스터 전체에서 하나의 인스턴스만 알림 전송
```

## 11. 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `alertmanager_nflog_gc_duration_seconds` | Summary | GC 소요시간 |
| `alertmanager_nflog_snapshot_duration_seconds` | Summary | 스냅샷 소요시간 |
| `alertmanager_nflog_snapshot_size_bytes` | Gauge | 스냅샷 크기 |
| `alertmanager_nflog_queries_total` | Counter | 쿼리 수 |
| `alertmanager_nflog_query_errors_total` | Counter | 쿼리 오류 수 |
| `alertmanager_nflog_query_duration_seconds` | Histogram | 쿼리 소요시간 |
| `alertmanager_nflog_propagated_messages_total` | Counter | 전파된 메시지 수 |
| `alertmanager_nflog_maintenance_total` | Counter | 유지보수 실행 수 |
| `alertmanager_nflog_maintenance_errors_total` | Counter | 유지보수 오류 수 |

## 12. 핵심 알고리즘 요약

```
┌─────────────────────────────────────────────┐
│           nflog 핵심 흐름                    │
│                                             │
│  [전송 성공]                                 │
│      │                                      │
│      ▼                                      │
│  SetNotifiesStage                           │
│      │                                      │
│      ├→ nflog.Log(entry)                    │
│      │   └→ st[key] = entry                 │
│      │                                      │
│      └→ broadcast(entry)                    │
│          └→ 클러스터 전파                     │
│                                             │
│  [다음 flush]                                │
│      │                                      │
│      ▼                                      │
│  DedupStage                                 │
│      │                                      │
│      ├→ nflog.Query(receiver, groupKey)     │
│      │   └→ 이전 발송 기록 반환              │
│      │                                      │
│      ├→ firing/resolved 변경 감지            │
│      │                                      │
│      └→ RepeatInterval 경과 확인             │
│          ├→ 변경 있음 → 통과                 │
│          ├→ 미경과 → 필터링 (중복)           │
│          └→ 경과 → 통과 (반복 전송)          │
└─────────────────────────────────────────────┘
```

## 13. 실제 소스 코드 심화 분석

### 13.1 Log 구조체 전체

```go
// nflog/nflog.go
type Log struct {
    clock      quartz.Clock          // 테스트 가능한 시계
    logger     *slog.Logger
    metrics    *metrics
    retention  time.Duration         // 만료 후 보존 기간

    mtx        sync.RWMutex
    st         state                 // map[string]*pb.MeshEntry
    broadcast  func([]byte)          // 클러스터 브로드캐스트

    maintenanceRunning atomic.Int32  // 동시 Maintenance 방지
}
```

`quartz.Clock`은 `coder/quartz` 패키지의 테스트 가능한 시계이다. 테스트에서 시간을 제어하여 GC, 만료, RepeatInterval 등의 시간 기반 로직을 결정적으로 테스트할 수 있다.

### 13.2 Store — key-value 저장소 상세

```go
// nflog/nflog.go
func NewStore(entry *pb.Entry) *Store {
    var receiverData map[string]*pb.ReceiverDataValue
    if entry != nil {
        receiverData = maps.Clone(entry.ReceiverData)  // 방어적 복사
    }
    if receiverData == nil {
        receiverData = make(map[string]*pb.ReceiverDataValue)
    }
    return &Store{data: receiverData}
}
```

**왜 `maps.Clone`으로 방어적 복사를 하는가?**

Store는 Pipeline의 여러 Stage에서 공유된다. 한 Stage가 Store를 수정하면 다른 Stage에 영향을 줄 수 있다. 복사본을 만들어 원본 Entry의 데이터를 보호한다.

### 13.3 에러 타입

```go
// nflog/nflog.go
var ErrNotFound = errors.New("not found")
var ErrInvalidState = errors.New("invalid state")
```

| 에러 | 발생 시점 | 의미 |
|------|----------|------|
| `ErrNotFound` | `Query()` 결과 없음 | 해당 키의 발송 기록 없음 |
| `ErrInvalidState` | `Merge()` 시 | 유효하지 않은 상태 데이터 수신 |

### 13.4 QueryParam 함수형 패턴

```go
// nflog/nflog.go
type query struct {
    recv     *pb.Receiver
    groupKey string
}

type QueryParam func(*query) error

func QReceiver(r *pb.Receiver) QueryParam {
    return func(q *query) error {
        q.recv = r
        return nil
    }
}

func QGroupKey(gk string) QueryParam {
    return func(q *query) error {
        q.groupKey = gk
        return nil
    }
}
```

**왜 함수형 옵션 패턴을 사용하는가?**

쿼리 파라미터가 추가될 때 기존 API를 변경하지 않고 새 `QueryParam` 함수만 추가하면 된다. 호출자는 필요한 파라미터만 선택적으로 전달한다.

---

## 14. 스냅샷 형식

```
스냅샷 파일 형식: Protobuf length-delimited 레코드
    ┌─────────────────────────┐
    │ varint: 레코드 1 크기    │
    │ MeshEntry 1 (protobuf)  │
    ├─────────────────────────┤
    │ varint: 레코드 2 크기    │
    │ MeshEntry 2 (protobuf)  │
    ├─────────────────────────┤
    │ ...                     │
    └─────────────────────────┘

    protodelim.MarshalTo()로 직렬화
    protodelim.UnmarshalFrom()으로 역직렬화
```

`google.golang.org/protobuf/encoding/protodelim` 패키지를 사용하여 길이 접두사가 붙는 Protobuf 레코드를 연속으로 기록한다. 이 형식은 스트리밍 읽기가 가능하여, 전체 파일을 메모리에 로드하지 않고도 레코드를 하나씩 읽을 수 있다.

---

## 15. Maintenance 안전장치

```go
// nflog/nflog.go
func (l *Log) Maintenance(interval time.Duration, snapf string,
    stopc <-chan struct{}, override MaintenanceFunc) {

    if !l.maintenanceRunning.CompareAndSwap(0, 1) {
        return  // 이미 실행 중이면 중복 실행 방지
    }
    // ...
}
```

**왜 `atomic.Int32`로 중복 방지를 하는가?**

설정 리로드 시 Maintenance goroutine이 재시작될 수 있다. CAS(Compare-And-Swap) 연산으로 하나의 Maintenance만 실행되도록 보장한다.

---

## 16. 성능 고려사항

| 항목 | 설계 | 이유 |
|------|------|------|
| state는 map | O(1) 조회 | DedupStage에서 매 flush마다 조회 |
| GC는 주기적 | ticker 기반 | 연속적 GC는 과도한 잠금 유발 |
| 스냅샷은 별도 goroutine | Maintenance에서 GC 후 수행 | 메인 처리 경로에 영향 최소화 |
| broadcast는 비동기 | TransmitLimitedQueue | 네트워크 지연이 로컬 처리를 차단하지 않음 |
| retention 기반 만료 | 시간 기반 TTL | 무한 증가 방지 |

---

## 17. 운영 가이드

### 17.1 스냅샷 파일 위치

```
{--storage.path}/nflog  (기본: data/nflog)
```

### 17.2 retention 설정

```bash
alertmanager --data.retention=120h  # 기본 120시간 (5일)
```

retention이 너무 짧으면 클러스터 파티션 후 복구 시 중복 알림이 발생할 수 있다. 너무 길면 스냅샷 크기가 증가한다.

### 17.3 모니터링 권장 쿼리

```promql
# GC 빈도와 소요시간
rate(alertmanager_nflog_gc_duration_seconds_count[5m])

# 스냅샷 크기 추이
alertmanager_nflog_snapshot_size_bytes

# 쿼리 에러율
rate(alertmanager_nflog_query_errors_total[5m])
  / rate(alertmanager_nflog_queries_total[5m])
```

## 18. 운영 시 주요 고려사항

### 18.1 Notification Log 크기 관리

Notification Log의 크기는 알림 빈도와 직접적으로 비례한다. 대규모 환경에서는 다음 요소를 고려해야 한다:

| 파라미터 | 기본값 | 설명 |
|----------|--------|------|
| `--data.retention` | 120h | 알림 로그 보존 기간 |
| GC 주기 | 자동 | retention 기간 초과 엔트리 자동 정리 |
| 스냅샷 주기 | 자동 | 메모리 → 디스크 직렬화 주기 |

### 18.2 클러스터 환경에서의 동기화

클러스터 모드에서 Notification Log는 Gossip 프로토콜을 통해 피어 간 동기화된다. 각 엔트리는 `nflog/nflogpb.Entry` 프로토콜 버퍼로 직렬화되어 전파되며, 수렴(convergence)까지의 지연시간은 클러스터 크기에 따라 달라진다.

```
nflog/nflog.go → Log.GC() → retention 기반 만료 엔트리 제거
nflog/nflog.go → Log.Snapshot() → protobuf 직렬화 → 디스크 저장
nflog/nflog.go → Log.Merge() → Gossip으로 수신된 원격 엔트리 병합
```

이 설계는 최종 일관성(eventual consistency) 모델을 따르며, 네트워크 파티션 상황에서도 각 노드가 독립적으로 중복 억제 판단을 수행할 수 있다.
