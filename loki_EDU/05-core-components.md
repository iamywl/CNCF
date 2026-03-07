# Loki 핵심 컴포넌트

## 1. 컴포넌트 관계도

```
                    ┌─────────────────────────────────────────────┐
                    │              Loki Server                    │
                    │                                             │
  Push API ────────▶│ ┌─────────────┐     ┌─────────────┐        │
                    │ │ Distributor │────▶│  Ingester   │        │
                    │ └──────┬──────┘     └──────┬──────┘        │
                    │        │                   │               │
                    │        │ Ring               │ Flush         │
                    │        ▼                   ▼               │
                    │   ┌─────────┐        ┌──────────┐          │
                    │   │  Ring   │        │  Store   │          │
                    │   └─────────┘        └──────────┘          │
                    │                           ▲                │
  Query API ───────▶│ ┌───────────────┐         │                │
                    │ │Query Frontend │    ┌─────┴─────┐         │
                    │ └───────┬───────┘    │  Querier  │         │
                    │         │            └───────────┘         │
                    │         ▼                  ▲               │
                    │   ┌───────────────┐        │               │
                    │   │Query Scheduler│────────┘               │
                    │   └───────────────┘                        │
                    │                                             │
                    │   ┌──────────┐ ┌───────────┐ ┌──────────┐  │
                    │   │Compactor │ │Index GW   │ │  Ruler   │  │
                    │   └──────────┘ └───────────┘ └──────────┘  │
                    └─────────────────────────────────────────────┘
```

---

## 2. Distributor

### 2.1 역할
클라이언트(Alloy, Promtail)로부터 로그를 수신하고, 검증한 뒤 Ring을 통해 적절한 Ingester에 분배한다.

### 2.2 핵심 구조

소스: `pkg/distributor/distributor.go:158-228`

```go
type Distributor struct {
    cfg                  Config
    ingestersRing        ring.ReadRing          // Ingester 해시 링
    ingestionRateLimiter *limiter.RateLimiter   // 테넌트별 수집 속도 제한
    validator            *Validator             // 입력 검증
    ingesterClients      *ring_client.Pool      // Ingester gRPC 연결 풀
    partitionRing        ring.PartitionRingReader // Kafka 파티션 라우팅
    labelParserCache     map[string]parsedLabels  // 레이블 파싱 캐시
}
```

### 2.3 처리 파이프라인

```
PushRequest 수신
│
├── 1. 레이블 파싱 (parseStreamLabels)
│     ├── 레이블 문자열 → labels.Labels
│     ├── enforced labels 확인 (필수 레이블)
│     └── 파싱 캐시 활용 (동일 레이블 재사용)
│
├── 2. 엔트리 검증 (validateEntry)
│     ├── 타임스탬프 범위 확인 (미래/과거 제한)
│     ├── 라인 길이 확인 (max_line_size)
│     ├── 레이블 수/크기 확인
│     └── 구조화 메타데이터 검증
│
├── 3. 레이트 리밋
│     ├── ingestionRateLimiter.AllowN(tenant, bytes)
│     ├── 로컬 + 글로벌 제한 동시 적용
│     └── 초과 시 HTTP 429 반환
│
├── 4. Ring 라우팅
│     ├── Stream.Hash → Ring.Get() → ReplicationSet
│     ├── Replication Factor만큼 복제
│     └── streamsByIngester 맵 구성
│
└── 5. 병렬 전송
      ├── Ingester별 goroutine 생성
      ├── gRPC Push() 호출
      └── minSuccess 성공 시 응답 반환
```

### 2.4 레이트 리밋 전략

| 전략 | 설명 |
|------|------|
| **로컬** | 각 Distributor 인스턴스에서 독립 적용 |
| **글로벌** | 모든 Distributor의 합산 속도 제한 (Rate Store 기반) |

글로벌 전략에서는 RateStore가 모든 Ingester의 수집 속도를 주기적으로 수집하여 정확한 전체 속도를 계산한다.

---

## 3. Ingester

### 3.1 역할
Distributor로부터 로그를 수신하여 인메모리에 청크 단위로 버퍼링하고, 주기적으로 오브젝트 스토리지에 플러시한다.

### 3.2 핵심 구조

소스: `pkg/ingester/ingester.go:237`

```go
type Ingester struct {
    cfg              Config
    instances        map[string]*instance     // 테넌트별 인스턴스
    instancesMtx     sync.RWMutex
    lifecycler       *ring.Lifecycler         // Ring 멤버십 관리
    flushQueues      []*util.PriorityQueue    // 플러시 워커 큐
    flushRateLimiter *rate.Limiter            // 플러시 속도 제한
    wal              WAL                      // Write-Ahead Log
    readOnly         atomic.Bool              // 읽기 전용 모드
}
```

### 3.3 테넌트 격리

```
Ingester
└── instances map[string]*instance
      │
      ├── "tenant-a" → instance
      │     ├── streams: streamsMap (fp → stream)
      │     ├── index: InvertedIndex
      │     └── wal: WAL
      │
      └── "tenant-b" → instance
            ├── streams: streamsMap (fp → stream)
            ├── index: InvertedIndex
            └── wal: WAL
```

### 3.4 스트림 관리

소스: `pkg/ingester/stream.go:41-89`

```
stream.Push(entries)
│
├── 중복 검사
│     └── lastLine(ts, content) 비교
│
├── 순서 확인
│     ├── unorderedWrites=true → 무조건 수용
│     └── unorderedWrites=false → ts > lastLine.ts 필수
│
├── MemChunk.Append(entry)
│     ├── HeadBlock에 비압축 추가
│     ├── HeadBlock 크기 > blockSize → 압축 → blocks[]
│     └── 전체 크기 > targetSize → 청크 닫기
│
├── WAL Record 기록
│
└── tailers에게 브로드캐스트
      └── 실시간 테일링 클라이언트에 전달
```

### 3.5 플러시 메커니즘

소스: `pkg/ingester/flush.go`

```
┌───────────────────────────────────────────────────┐
│              Flush Architecture                    │
│                                                   │
│  sweepUsers() ─────▶ sweepStream() ──▶ flushOp   │
│  (주기적 스캔)        (조건 확인)       (큐 삽입)    │
│                                                   │
│  ┌─────────────────────────────────┐              │
│  │ flushQueues[0]  flushQueues[1]  │              │
│  │   ├── flushOp    ├── flushOp   │              │
│  │   └── flushOp    └── flushOp   │              │
│  └──────┬───────────────┬─────────┘              │
│         │               │                         │
│    flushLoop(0)    flushLoop(1)                   │
│    (워커 고루틴)    (워커 고루틴)                    │
│         │               │                         │
│    flushRateLimiter.Wait() ← 속도 제한             │
│         │               │                         │
│    store.Put(chunks) ← 오브젝트 스토리지 업로드      │
└───────────────────────────────────────────────────┘
```

**플러시 조건:**
- 청크가 닫혔을 때 (`closed = true`)
- 청크가 `MaxChunkAge`를 초과
- 청크가 `MaxChunkIdle` 동안 업데이트 없음
- 강제 플러시 (종료, HTTP 엔드포인트)
- 소유권 변경 (Ring 토폴로지 변경)

### 3.6 WAL (Write-Ahead Log)

```
목적: 장애 시 인메모리 데이터 복구

쓰기 흐름:
  instance.Push() → WAL.Log(record)
  └── 디스크에 순차 기록

복구 흐름:
  Ingester 시작 → WAL.Open()
  ├── Checkpoint 로드 (스냅샷)
  └── Segment 재생 (증분)
      └── entryCt로 중복 감지
```

---

## 4. Querier

### 4.1 역할
LogQL 쿼리를 실행하고, Ingester(최근 데이터)와 Store(과거 데이터)에서 결과를 병합한다.

### 4.2 핵심 구조

소스: `pkg/querier/querier.go:52-67`

```go
type Config struct {
    TailMaxDuration           time.Duration // 테일링 최대 지속 시간
    ExtraQueryDelay           time.Duration // 추가 쿼리 지연
    QueryIngestersWithin      time.Duration // Ingester 쿼리 시간 범위 (기본 3h)
    MaxConcurrent             int           // 최대 동시 쿼리 수
    QueryStoreOnly            bool          // Store만 쿼리
    QueryIngesterOnly         bool          // Ingester만 쿼리
    MultiTenantQueriesEnabled bool          // 멀티테넌트 쿼리 허용
}
```

### 4.3 듀얼 소스 쿼리

```
시간 축:
  ──────────────────────────────────────────────▶ 현재
  │◀─── Store 쿼리 범위 ──▶│◀── Ingester 범위 ──▶│
  │                        │                    │
  start                 now-3h                 end

Querier.SelectLogs():
  ├── storeQueryInterval: [start, min(end, now)]
  │     └── store.SelectLogs() → TSDB → ChunkRef → Object Store
  │
  ├── ingesterQueryInterval: [max(start, now-3h), end]
  │     └── ingesterQuerier.SelectLogs() → gRPC → instance.Query()
  │
  └── MergeEntryIterator(storeIter, ingesterIter, direction)
        └── 타임스탬프 기준 정렬 병합
```

### 4.4 반복자 계층

```
MergeEntryIterator
├── IngesterEntryIterator
│     └── stream.Iterator(from, through, direction, pipeline)
│           └── MemChunk.Iterator()
│                 └── block.Iterator() × N + head.Iterator()
│
└── StoreEntryIterator
      └── ChunkIterator
            └── MemChunk (디코딩) → Iterator()
```

---

## 5. Query Frontend

### 5.1 역할
쿼리 요청을 수신하여 캐싱, 시간 분할, 쿼리 샤딩을 적용한 뒤 Querier에 분배한다.

### 5.2 쿼리 최적화

```
클라이언트 쿼리: {app="api"} |= "error" [지난 24시간]
│
├── 1. 캐시 확인
│     └── 이전에 동일 쿼리 실행 → 캐시 결과 반환
│
├── 2. 시간 분할 (Split by Interval)
│     └── 24시간 → 24개의 1시간 서브쿼리로 분할
│
├── 3. 쿼리 샤딩 (Shard by Label)
│     └── 각 서브쿼리를 N개 샤드로 분할
│         (레이블 해시 기반)
│
├── 4. 큐잉
│     └── requestQueue.Enqueue(tenantID, subQuery)
│         └── 테넌트별 공정 큐
│
└── 5. 결과 병합
      └── 모든 서브쿼리 결과를 시간순 병합
```

### 5.3 Frontend-Scheduler-Querier 연결

```
Frontend (Producer)          Scheduler (Queue)        Querier (Consumer)
     │                            │                        │
     │ Enqueue(request)           │                        │
     │───────────────────────────▶│                        │
     │                            │                        │
     │                            │ RegisterConsumer()     │
     │                            │◀───────────────────────│
     │                            │                        │
     │                            │ Dequeue() → request    │
     │                            │───────────────────────▶│
     │                            │                        │
     │                            │                        │ SelectLogs()
     │                            │                        │
     │                            │ response               │
     │◀──────────────────────────────────────────────────────│
```

---

## 6. Compactor

### 6.1 역할
TSDB 인덱스 파일을 압축/병합하고, 보존 정책에 따라 만료된 데이터를 삭제한다.

### 6.2 주요 기능

| 기능 | 설명 |
|------|------|
| **인덱스 압축** | 여러 소규모 인덱스 파일을 하나로 병합 |
| **보존 정책** | 글로벌/테넌트별 데이터 보존 기간 적용 |
| **삭제 처리** | 삭제 요청에 따른 청크/인덱스 정리 |
| **테이블 관리** | period별 테이블 생성/삭제 |

### 6.3 압축 프로세스

```
Compactor (주기적 실행)
│
├── for table in tables:
│     ├── 테이블의 모든 인덱스 파일 목록
│     ├── 테넌트별 그룹화
│     └── for tenant in tenants:
│           ├── 인덱스 파일들 다운로드
│           ├── 병합 (중복 제거, 정렬)
│           ├── 보존 정책 적용 (만료 청크 참조 제거)
│           └── 압축된 인덱스 업로드
│
└── 삭제 요청 처리
      ├── 삭제 마커 확인
      ├── 해당 청크 참조 제거
      └── 오브젝트 스토리지에서 청크 삭제
```

---

## 7. Ruler

### 7.1 역할
LogQL 기반 알림 규칙과 녹화 규칙을 주기적으로 평가한다.

### 7.2 동작 방식

```
Ruler
│
├── 규칙 저장소에서 규칙 로드
│     └── configstore (S3/GCS/Local/K8s ConfigMap)
│
├── 평가 루프 (evaluation_interval마다)
│     ├── 각 규칙 그룹 순회
│     ├── LogQL 쿼리 실행
│     │     ├── Local 모드: 직접 Querier 호출
│     │     └── Remote 모드: Query Frontend 통해 실행
│     │
│     ├── 알림 규칙:
│     │     ├── 결과가 조건 충족 → Alert 생성
│     │     └── Alertmanager로 Alert 전송
│     │
│     └── 녹화 규칙:
│           └── 결과를 새 시계열로 저장
│
└── 규칙 API (CRUD)
      ├── GET /loki/api/v1/rules
      ├── POST /loki/api/v1/rules/{namespace}
      └── DELETE /loki/api/v1/rules/{namespace}/{group}
```

---

## 8. Index Gateway

### 8.1 역할
Querier 대신 인덱스 조회를 수행하는 전용 서비스. Querier의 인덱스 캐시 부담을 오프로드한다.

### 8.2 아키텍처

```
기존 (Index Gateway 없이):
  Querier → TSDB Index (Object Store) → 직접 조회

Index Gateway 도입 후:
  Querier → Index Gateway → TSDB Index
                  │
                  └── 인덱스 캐시 (메모리/디스크)
```

**장점:**
- Querier가 인덱스를 로컬에 캐시할 필요 없음
- 인덱스 조회 전용 리소스 할당 가능
- Ring 기반 샤딩으로 인덱스를 분산 관리

---

## 9. Ring (dskit)

### 9.1 역할
Consistent Hash Ring을 사용한 서비스 디스커버리와 데이터 분산.

### 9.2 핵심 동작

```
Ring 구조:
         Token 0          Token 100        Token 200
  ──────────┼────────────────┼────────────────┼──────
            │ Ingester-1     │ Ingester-2     │ Ingester-3
            │                │                │

Stream 라우팅:
  1. hash(tenant + labels) = 150
  2. Ring에서 150 이상의 첫 토큰 → Token 200 → Ingester-3
  3. RF=3이면 Ingester-3, 1, 2에 복제

KV Store (Ring 상태 저장):
  ├── memberlist (기본): 가십 프로토콜로 분산
  ├── consul: Consul KV
  └── etcd: etcd KV
```

### 9.3 Ring 참여 모듈

| 모듈 | Ring 용도 |
|------|----------|
| Ingester | 스트림 소유권 관리 |
| Distributor | Ingester Ring 조회 (쓰기 라우팅) |
| Query Scheduler | Ring 기반 프론트엔드 연결 |
| Index Gateway | 인덱스 샤딩 |
| Compactor | 리더 선출 (단일 실행 보장) |
| Ruler | 규칙 그룹 분산 |

---

## 10. 스토리지 백엔드

### 10.1 계층 구조

```
┌─────────────────────────────────────┐
│           Storage API               │
│    (pkg/storage/store.go)           │
├─────────────────┬───────────────────┤
│   청크 스토어    │   인덱스 스토어     │
│                 │                   │
│  Object Store   │   TSDB Index      │
│  ┌───────────┐  │  ┌─────────────┐  │
│  │ S3        │  │  │ TSDB (파일)  │  │
│  │ GCS       │  │  │ BoltDB      │  │
│  │ Azure     │  │  │             │  │
│  │ Filesystem│  │  │             │  │
│  └───────────┘  │  └─────────────┘  │
├─────────────────┴───────────────────┤
│             캐시 계층                │
│  ┌──────┐ ┌────────┐ ┌───────────┐  │
│  │ 인덱스│ │ 청크   │ │ 쿼리 결과 │  │
│  │ 캐시  │ │ 캐시   │ │ 캐시      │  │
│  └──────┘ └────────┘ └───────────┘  │
│  (memcached / redis / embedded)      │
└─────────────────────────────────────┘
```

### 10.2 스키마 설정

```yaml
schema_config:
  configs:
    - from: 2024-01-01
      store: tsdb           # 인덱스 타입
      object_store: s3       # 청크 저장소
      schema: v13            # 스키마 버전
      index:
        prefix: index_
        period: 24h          # 인덱스 테이블 주기
```

---

## 11. 컴포넌트 간 통신

| 송신자 | 수신자 | 프로토콜 | 서비스 |
|--------|--------|---------|--------|
| Client | Distributor | HTTP | `POST /loki/api/v1/push` |
| Client | Query Frontend | HTTP | `GET /loki/api/v1/query_range` |
| Distributor | Ingester | gRPC | `Pusher.Push()` |
| Querier | Ingester | gRPC | `Querier.Query()` |
| Query Frontend | Query Scheduler | gRPC | 큐 삽입/삭제 |
| Query Scheduler | Querier | gRPC | `SchedulerForQuerierProcessServer` |
| Querier | Index Gateway | gRPC | `IndexGateway.GetChunkRef()` |
| Ruler | Alertmanager | HTTP | `POST /api/v2/alerts` |
| All | Ring (KV) | Memberlist/HTTP | 해시 링 동기화 |

---

## 12. 참고 자료

- Distributor: `pkg/distributor/distributor.go`
- Ingester: `pkg/ingester/ingester.go`, `pkg/ingester/flush.go`
- Querier: `pkg/querier/querier.go`
- Query Frontend: `pkg/lokifrontend/frontend/v1/frontend.go`
- Compactor: `pkg/compactor/compactor.go`
- Ruler: `pkg/ruler/ruler.go`
- Index Gateway: `pkg/indexgateway/gateway.go`
- Ring: dskit 패키지 (`github.com/grafana/dskit/ring`)
