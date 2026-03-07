# Loki 시퀀스 다이어그램

## 1. 로그 쓰기 흐름 (Push)

### 1.1 전체 흐름

```
Client          Distributor              Ring           Ingester           Storage
  │                  │                     │                │                  │
  │ POST /push       │                     │                │                  │
  │ PushRequest      │                     │                │                  │
  │─────────────────▶│                     │                │                  │
  │                  │                     │                │                  │
  │                  │ parseStreamLabels() │                │                  │
  │                  │ validateEntry()     │                │                  │
  │                  │ rateLimiter.AllowN()│                │                  │
  │                  │                     │                │                  │
  │                  │ Get(stream.HashKey) │                │                  │
  │                  │────────────────────▶│                │                  │
  │                  │                     │                │                  │
  │                  │  ReplicationSet     │                │                  │
  │                  │◀────────────────────│                │                  │
  │                  │                     │                │                  │
  │                  │ gRPC Push (per ingester, concurrent) │                  │
  │                  │─────────────────────────────────────▶│                  │
  │                  │                     │                │                  │
  │                  │                     │                │ GetOrCreate       │
  │                  │                     │                │ Instance(tenant)  │
  │                  │                     │                │                  │
  │                  │                     │                │ stream.Push()     │
  │                  │                     │                │ MemChunk.Append() │
  │                  │                     │                │                  │
  │                  │                     │                │ WAL.Log(record)   │
  │                  │                     │                │                  │
  │                  │      PushResponse   │                │                  │
  │                  │◀─────────────────────────────────────│                  │
  │                  │                     │                │                  │
  │  PushResponse    │                     │                │                  │
  │◀─────────────────│                     │                │                  │
  │                  │                     │                │                  │
  │                  │        [주기적 플러시]  │                │                  │
  │                  │                     │                │ sweepStream()     │
  │                  │                     │                │ flushOp()         │
  │                  │                     │                │────────────────▶  │
  │                  │                     │                │  ChunkStore.Put() │
  │                  │                     │                │  Index.Write()    │
```

### 1.2 Distributor 상세

소스: `pkg/distributor/distributor.go:552-933`

```
PushWithResolver()
│
├── 1. 스트림 파싱 및 검증 (Lines 584-752)
│     ├── parseStreamLabels(): 레이블 문자열 → labels.Labels 파싱
│     ├── enforcedLabels 확인
│     ├── validateEntry(): 타임스탬프 범위, 라인 길이, 레이블 수 검증
│     ├── discoverLogLevels(): 로그 레벨 자동 감지
│     └── structuredMetadata 유효성 검증
│
├── 2. 레이트 리밋 확인 (Lines 793-796)
│     └── ingestionRateLimiter.AllowN(tenantID, totalBytes)
│         ├── 허용 → 계속 진행
│         └── 거부 → HTTP 429 반환, discardedBytes 메트릭 증가
│
├── 3. Ring 조회 및 스트림 분배 (Lines 870-894)
│     └── for stream in streams:
│           ├── ingestersRing.Get(stream.HashKey, WriteNoExtend)
│           │     → ReplicationSet{Instances, MaxErrors}
│           ├── minSuccess = len(Instances) - MaxErrors
│           └── streamsByIngester[addr] = append(stream)
│
└── 4. Ingester 전송 (Lines 900-932)
      └── for ingester, streams in streamsByIngester:
            └── goroutine:
                  ├── context.WithTimeout(RemoteTimeout)
                  ├── ingesterClient.Push(ctx, streams)
                  └── tracker.record(success/failure)
```

### 1.3 Ingester Push 상세

소스: `pkg/ingester/ingester.go:998`, `pkg/ingester/instance.go:231`

```
Ingester.Push(ctx, req)
│
├── readOnly 상태 확인 → 읽기 전용이면 503 반환
│
├── GetOrCreateInstance(tenantID)
│     ├── instances[tenantID] 존재 → 반환
│     └── 없으면 newInstance() 생성 + 등록
│
└── instance.Push(ctx, req)
      │
      ├── WAL record 할당 (pool에서)
      │
      ├── for stream in req.Streams:
      │     ├── streams.LoadOrStoreNew(labels, fp)
      │     │     ├── 기존 스트림 → 반환
      │     │     └── 새 스트림 → createStream()
      │     │           ├── 스트림 수 제한 확인
      │     │           ├── 역인덱스에 등록
      │     │           └── 청크 인코딩 설정
      │     │
      │     ├── stream.chunkMtx.Lock()
      │     │
      │     └── stream.Push(ctx, entries, record)
      │           ├── 중복 검사 (lastLine과 비교)
      │           ├── MemChunk.Append(entry)
      │           │     ├── HeadBlock에 추가
      │           │     └── blockSize 도달 → 압축 → blocks[]에 추가
      │           └── tailer들에게 브로드캐스트
      │
      └── WAL.Log(record) → 디스크에 영구 기록
```

---

## 2. 로그 읽기 흐름 (Query)

### 2.1 전체 흐름

```
Client       QueryFrontend     QueryScheduler      Querier        Ingester    Store
  │               │                  │                 │              │          │
  │ GET /query    │                  │                 │              │          │
  │ LogQL         │                  │                 │              │          │
  │──────────────▶│                  │                 │              │          │
  │               │                  │                 │              │          │
  │               │ 시간 분할          │                 │              │          │
  │               │ 쿼리 샤딩         │                 │              │          │
  │               │ 캐시 확인          │                 │              │          │
  │               │                  │                 │              │          │
  │               │ Enqueue(request) │                 │              │          │
  │               │─────────────────▶│                 │              │          │
  │               │                  │                 │              │          │
  │               │                  │ Dequeue()       │              │          │
  │               │                  │────────────────▶│              │          │
  │               │                  │                 │              │          │
  │               │                  │                 │ SelectLogs() │          │
  │               │                  │                 │              │          │
  │               │                  │                 │ 시간 분할:      │          │
  │               │                  │                 │ 최근 3h 이내   │          │
  │               │                  │                 │─────────────▶│          │
  │               │                  │                 │              │          │
  │               │                  │                 │  EntryIter   │          │
  │               │                  │                 │◀─────────────│          │
  │               │                  │                 │              │          │
  │               │                  │                 │ 3h 이전        │          │
  │               │                  │                 │─────────────────────────▶│
  │               │                  │                 │              │          │
  │               │                  │                 │  EntryIter   │          │
  │               │                  │                 │◀─────────────────────────│
  │               │                  │                 │              │          │
  │               │                  │                 │ MergeIterator│          │
  │               │                  │                 │ (두 소스 병합)  │          │
  │               │                  │                 │              │          │
  │               │                  │ QueryResponse   │              │          │
  │               │◀─────────────────────────────────────              │          │
  │               │                  │                 │              │          │
  │ QueryResponse │                  │                 │              │          │
  │◀──────────────│                  │                 │              │          │
```

### 2.2 Querier 상세

소스: `pkg/querier/querier.go:153-216`

```
SingleTenantQuerier.SelectLogs(ctx, params)
│
├── 1. 시간 구간 계산
│     buildQueryIntervals(params.Start, params.End)
│     ├── ingesterQueryInterval: [max(start, now-QueryIngestersWithin), end]
│     └── storeQueryInterval:    [start, min(end, now)]
│
├── 2. Ingester 쿼리 (최근 데이터)
│     if ingesterQueryInterval 유효 && !QueryStoreOnly:
│       ingesterQuerier.SelectLogs(ctx, params)
│       └── 각 Ingester에 gRPC Query() 호출
│           └── instance.Query() → stream.Iterator()
│
├── 3. Store 쿼리 (과거 데이터)
│     if storeQueryInterval 유효 && !QueryIngesterOnly:
│       store.SelectLogs(ctx, params)
│       └── TSDB Index → ChunkRef 목록
│           → Object Store에서 청크 다운로드
│           → MemChunk 디코딩 → Iterator
│
└── 4. 결과 병합
      if len(iters) == 1: return iters[0]
      else: iter.NewMergeEntryIterator(ctx, iters, direction)
           └── 타임스탬프 기준 정렬 병합
```

### 2.3 Query Frontend 상세

소스: `pkg/lokifrontend/frontend/v1/frontend.go:155-273`

```
Frontend.RoundTripGRPC(ctx, req)
│
├── 1. 요청 생성
│     request{
│       originalCtx: ctx,
│       request:     httpReq,
│       err:         make(chan error, 1),
│       response:    make(chan *httpgrpc.HTTPResponse, 1),
│     }
│
├── 2. 큐에 삽입
│     queueRequest(ctx, &request)
│     └── requestQueue.Enqueue(tenantID, nil, &request)
│         └── MaxOutstandingPerTenant 초과 시 429 반환
│
└── 3. 응답 대기
      select {
        case <-ctx.Done():      → 취소됨
        case resp := <-response → 성공 응답
        case err := <-err       → 에러
      }

--- Worker 측 (Querier가 연결) ---

Frontend.Process(server)  // gRPC 스트림
│
├── RegisterConsumerConnection(querierID)
│
├── Loop:
│     ├── Dequeue(ctx, lastIndex, querierID)
│     │     → 만료되지 않은 요청 반환
│     │
│     ├── 만료 확인: req.originalCtx.Err()
│     │     └── 만료됨 → lastIndex.ReuseLastIndex() → 재시도
│     │
│     ├── server.Send(FrontendToClient{HttpRequest})
│     │     → Querier에 요청 전송
│     │
│     ├── server.Recv() → ClientToFrontend{HttpResponse}
│     │     → Querier로부터 응답 수신
│     │
│     └── req.response <- resp.HttpResponse
│           → 원래 클라이언트에 응답 전달
│
└── UnregisterConsumerConnection(querierID)
```

---

## 3. 청크 플러시 흐름

소스: `pkg/ingester/flush.go:106-268`

```
Ingester (주기적 또는 종료 시)
│
├── sweepUsers(immediate, mayRemoveStreams)
│     └── for tenant in instances:
│           └── for stream in tenant.streams:
│                 └── sweepStream(instance, stream, immediate)
│
├── sweepStream()
│     │
│     ├── 플러시 조건 확인:
│     │     ├── len(chunks) > 1                    (이전 청크 존재)
│     │     ├── immediate == true                  (강제 플러시)
│     │     ├── chunk age > MaxChunkAge            (최대 수명 초과)
│     │     ├── chunk idle > MaxChunkIdle           (유휴 시간 초과)
│     │     └── stream not owned by this ingester   (소유권 변경)
│     │
│     └── flushQueues[fp % ConcurrentFlushes].Enqueue(flushOp)
│
├── flushLoop(workerID)  // 워커 고루틴 (N개)
│     │
│     ├── queue.Dequeue() → flushOp
│     │
│     ├── flushRateLimiter.Wait(ctx)  // 속도 제한
│     │
│     └── flushOp()
│           │
│           └── flushUserSeries(userID, fp, immediate)
│                 │
│                 ├── collectChunksToFlush()
│                 │     → closed/aged/idle 청크 수집
│                 │
│                 ├── store.Put(ctx, chunks)
│                 │     ├── 청크 직렬화 (Bytes())
│                 │     ├── Object Store 업로드
│                 │     └── TSDB Index에 ChunkMeta 기록
│                 │
│                 └── stream.reportFlushResult()
│                       ├── 성공 → 메트릭 업데이트, 청크 제거
│                       └── 실패 → backoff 후 재시도
│
└── 플러시 이유별 메트릭:
      flushReasonIdle      │ 유휴
      flushReasonMaxAge    │ 최대 수명
      flushReasonForced    │ 강제 (종료)
      flushReasonNotOwned  │ 소유권 변경
      flushReasonFull      │ 용량 초과
      flushReasonSynced    │ WAL 동기화
```

---

## 4. Ingester 종료 흐름

소스: `pkg/ingester/ingester.go:813-994`

```
SIGTERM 수신 또는 /ingester/shutdown 호출
│
├── PrepareShutdown (선택적, 사전 준비)
│     ├── POST /ingester/prepare_shutdown
│     │     ├── 마커 파일 생성
│     │     ├── lifecycler.SetFlushOnShutdown(true)
│     │     ├── lifecycler.SetUnregisterOnShutdown(true)
│     │     └── terminateOnShutdown = true
│     │
│     └── [로드밸런서가 트래픽 제거 대기]
│
├── ShutdownHandler / handleShutdown()
│     ├── flush=true: 모든 인메모리 데이터 플러시
│     ├── delete_ring_tokens=true: Ring에서 토큰 제거
│     └── terminate=true: 프로세스 종료
│
├── Ingester.stopping()
│     ├── 쓰기 요청 거부 시작
│     ├── flush(true) → sweepUsers(true, false)
│     │     └── 모든 청크를 즉시 플러시
│     ├── flushQueues 종료
│     ├── flushQueuesDone.Wait() → 모든 워커 완료 대기
│     └── 리소스 해제
│
└── Ring에서 등록 해제
      └── lifecycler.Stop()
```

---

## 5. 실시간 테일링 흐름

```
Client                  Querier                Ingester
  │                        │                       │
  │ WebSocket /tail        │                       │
  │ {selector, limit}      │                       │
  │───────────────────────▶│                       │
  │                        │                       │
  │                        │ gRPC Tail(TailRequest) │
  │                        │──────────────────────▶│
  │                        │                       │
  │                        │                       │ tailer 등록
  │                        │                       │ stream.tailers[id] = tailer
  │                        │                       │
  │                        │                       │ [새 로그 도착 시]
  │                        │                       │ stream.Push() 내에서:
  │                        │                       │   tailer.send(entry)
  │                        │                       │
  │                        │ TailResponse (stream) │
  │                        │◀──────────────────────│
  │                        │                       │
  │ WebSocket frame        │                       │
  │◀───────────────────────│                       │
  │                        │                       │
  │ [연결 종료]              │                       │
  │                        │ context.Done()         │
  │                        │──────────────────────▶│
  │                        │                       │ tailer 제거
```

---

## 6. WAL 복구 흐름

```
Ingester 시작 (장애 후 재시작)
│
├── WAL 파일 발견
│     └── wal.Open(walDir)
│
├── Checkpoint 로드 (있으면)
│     └── 가장 최근 체크포인트의 스트림/엔트리 복구
│         └── instance.Push(entries)
│
├── Segment 재생
│     └── checkpoint 이후의 WAL 세그먼트 순서대로:
│           └── for record in segment:
│                 ├── record.entryCt > stream.entryCt → 적용
│                 └── record.entryCt <= stream.entryCt → 스킵 (이미 복구됨)
│
├── 복구 완료
│     └── Ring에 등록
│
└── 정상 운영 시작
      └── 새 WAL 세그먼트 생성
```

---

## 7. 멀티테넌트 쿼리 흐름

```
Query Frontend
│
├── X-Scope-OrgID: "tenant-a|tenant-b" (멀티테넌트 쿼리)
│
├── 테넌트 분리
│     └── for tenantID in split(orgID, "|"):
│           └── 개별 쿼리 생성 (각 테넌트별)
│
├── 각 테넌트별 쿼리 실행
│     ├── tenant-a → Querier → Ingester(a) + Store(a)
│     └── tenant-b → Querier → Ingester(b) + Store(b)
│
└── 결과 병합
      └── MergeIterator(tenant-a 결과, tenant-b 결과)
            → 타임스탬프 기준 정렬
```

---

## 8. 참고 자료

- Distributor Push: `pkg/distributor/distributor.go:552-933`
- Ingester Push: `pkg/ingester/ingester.go:998`, `pkg/ingester/instance.go:231`
- Ingester Flush: `pkg/ingester/flush.go:106-268`
- Querier SelectLogs: `pkg/querier/querier.go:153-216`
- Query Frontend: `pkg/lokifrontend/frontend/v1/frontend.go:155-273`
- Ingester Shutdown: `pkg/ingester/ingester.go:813-994`
