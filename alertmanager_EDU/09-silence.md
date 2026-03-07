# Alertmanager Silence 관리 Deep Dive

## 1. 개요

Silence는 특정 레이블 조건에 매칭되는 Alert를 일정 기간 동안 억제하는 메커니즘이다. `silence/silence.go`에 구현되어 있으며, Silences 저장소와 Silencer(Muter 구현)로 구성된다.

## 2. Silences 저장소

### 2.1 구조체

```go
// silence/silence.go
type Silences struct {
    clock     quartz.Clock        // 테스트 가능한 시계
    logger    *slog.Logger
    metrics   *metrics
    retention time.Duration       // 만료 후 보관 기간

    mtx       sync.RWMutex
    st        state               // 내부 상태: map[silenceID]*pb.MeshEntry
    version   int                 // 변경 추적 (캐시 무효화용)
    broadcast func([]byte)        // 클러스터 브로드캐스트 함수
    mi        matcherIndex        // 레이블 → Silence 인덱스
    vi        versionIndex        // Silence ID → 버전 인덱스
}
```

### 2.2 state 타입

```go
type state map[string]*pb.MeshEntry
```

키는 Silence ID(ULID), 값은 Protobuf MeshEntry이다. MeshEntry는 Silence 데이터와 만료 시간을 포함한다.

### 2.3 Limits

```go
// silence/silence.go
type Limits struct {
    MaxSilences         func() int    // 최대 Silence 수
    MaxSilenceSizeBytes func() int    // 최대 Silence 크기 (바이트)
}
```

## 3. Silence CRUD

### 3.1 Set() — 생성/업데이트

```
Silences.Set(silence *pb.Silence):
    1. 유효성 검증:
       - Matchers가 1개 이상인지
       - StartsAt < EndsAt인지
       - 각 Matcher의 Name, Value 유효성
    2. Limits 확인:
       - MaxSilences 초과 여부
       - MaxSilenceSizeBytes 초과 여부
    3. ID가 없으면 새 ULID 생성
    4. 기존 Silence 존재 확인 (업데이트 vs 새 생성)
    5. st[id] = MeshEntry{silence, expiresAt}
    6. matcherIndex 업데이트
    7. version++
    8. broadcast(직렬화된 Silence) → 클러스터 전파
    9. return silenceID
```

### 3.2 Expire() — 만료

```
Silences.Expire(ids ...string):
    각 ID에 대해:
    1. st에서 Silence 조회
    2. 이미 만료된 Silence는 건너뜀
    3. EndsAt = now (즉시 만료)
    4. UpdatedAt = now
    5. version++
    6. broadcast → 클러스터 전파
```

### 3.3 Query() — 조회

```
Silences.Query(params ...QueryParam):
    1. 쿼리 파라미터 적용:
       - QState(active, pending, expired)
       - QMatches(lset)
       - QIDs(ids...)
    2. st를 순회하며 조건에 맞는 Silence 수집
    3. 정렬 후 반환
```

## 4. Silencer (Muter 구현)

### 4.1 구조체

```go
// silence/silence.go
type Silencer struct {
    silences *Silences             // Silence 저장소
    cache    *cache                // fingerprint → silence ID 캐시
    marker   types.AlertMarker    // Alert 상태 마커
    logger   *slog.Logger
}
```

### 4.2 Mutes() 알고리즘

```go
// silence/silence.go
func (s *Silencer) Mutes(ctx context.Context, lset model.LabelSet) bool {
    fp := lset.Fingerprint()

    // 1. 캐시 확인 (버전 기반)
    activeIDs, ok := s.cache.Get(fp, s.silences.Version())
    if ok {
        // 캐시 HIT
        s.marker.SetActiveOrSilenced(fp, activeIDs)
        return len(activeIDs) > 0
    }

    // 2. 캐시 MISS → 모든 활성 Silence와 매칭
    activeIDs = s.silences.QueryActiveSilenceIDs(lset)

    // 3. 캐시 저장
    s.cache.Set(fp, s.silences.Version(), activeIDs)

    // 4. Marker 업데이트
    s.marker.SetActiveOrSilenced(fp, activeIDs)

    return len(activeIDs) > 0
}
```

### 4.3 캐시 메커니즘

```
┌──────────────────────────────────────┐
│            Silence Cache             │
│                                      │
│  map[Fingerprint]cacheEntry          │
│                                      │
│  cacheEntry:                         │
│    version    int                    │
│    silenceIDs []string               │
│                                      │
│  조회 로직:                           │
│  1. fp로 cacheEntry 조회             │
│  2. entry.version == Silences.version│
│     → HIT: silenceIDs 반환           │
│  3. version 불일치                   │
│     → MISS: 전체 매칭 재수행          │
│                                      │
│  무효화:                              │
│  - Silence 추가/삭제/만료 시          │
│    Silences.version++ 자동 증가      │
│  - 다음 Mutes() 호출에서 캐시 미스    │
│  - 점진적 갱신 (개별 fp별)           │
└──────────────────────────────────────┘
```

이 버전 기반 캐시는 전체 캐시 초기화 없이 개별 Alert의 캐시만 필요할 때 갱신하는 **lazy invalidation** 방식이다.

## 5. matcherIndex

Silence의 Matcher에서 사용되는 레이블 이름을 인덱스하여 빠른 조회를 지원한다:

```
matcherIndex:
    labelName → [silenceID1, silenceID2, ...]

    예:
    "alertname" → ["sil-001", "sil-003"]
    "severity"  → ["sil-002", "sil-003"]
```

Alert의 레이블과 Silence의 Matcher를 매칭할 때, 전체 Silence를 순회하지 않고 관련 Silence만 확인할 수 있다.

## 6. 스냅샷 (영속화)

### 6.1 Snapshot()

```
Silences.Snapshot(w io.Writer):
    1. st의 모든 MeshEntry 수집
    2. Protobuf로 직렬화
    3. w에 기록
    반환: 기록된 바이트 수
```

### 6.2 loadSnapshot()

```
Silences.loadSnapshot(r io.Reader):
    1. r에서 Protobuf 데이터 읽기
    2. 각 MeshEntry를 st에 복원
    3. matcherIndex 재구축
    4. version 초기화
```

### 6.3 Maintenance()

```
Silences.Maintenance(interval, snapf, stopc, override):
    주기적으로:
    1. GC() — 만료 후 retention 지난 Silence 삭제
    2. Snapshot() — 현재 상태를 파일로 저장
```

## 7. GC (Garbage Collection)

```
Silences.GC():
    현재 시간 = now

    st를 순회:
        if silence.EndsAt + retention < now:
            st에서 삭제
            matcherIndex에서 제거
            version++

    반환: 삭제된 Silence 수
```

retention 기본값은 `--data.retention` 플래그로 설정된다 (기본 120시간).

## 8. 클러스터 동기화

### 8.1 broadcast

Silence 생성/수정/삭제 시 직렬화된 데이터를 broadcast 함수로 전달:

```
Set() → Protobuf 직렬화 → broadcast(bytes)
                             ↓
                    Cluster Channel
                             ↓
                    Gossip 프로토콜로 전파
                             ↓
                    다른 인스턴스의 Merge()
```

### 8.2 Merge()

다른 인스턴스에서 전파된 Silence를 병합한다:

```
Silences.Merge(entries []*pb.MeshEntry):
    각 entry에 대해:
    1. 기존 st[id] 조회
    2. 없으면: 새로 추가
    3. 있으면: timestamp 비교
       - 수신 entry가 더 최신 → 업데이트
       - 기존이 더 최신 → 무시
    4. matcherIndex 업데이트
    5. version++
```

CRDT(Conflict-free Replicated Data Type) 방식으로, 최신 타임스탬프가 항상 승리한다.

## 9. Silence 상태 전이

```
생성 (API POST)
    │
    ├── StartsAt이 미래
    │   └── Pending (대기 중)
    │        │
    │        └── StartsAt 도달
    │             └── Active (활성)
    │
    ├── StartsAt이 현재/과거
    │   └── Active (활성)
    │
    └── Active 상태
         │
         ├── EndsAt 도달 → Expired (만료)
         ├── API DELETE → Expired (즉시 만료)
         │
         └── Expired
              │
              └── retention 경과 → GC에서 삭제
```

## 10. Silence Protobuf 메시지

```protobuf
// silence/silencepb/silence.proto
message Silence {
    string id = 1;
    repeated Matcher matchers = 2;
    google.protobuf.Timestamp starts_at = 3;
    google.protobuf.Timestamp ends_at = 4;
    google.protobuf.Timestamp updated_at = 5;
    string created_by = 6;
    string comment = 7;
}

message MeshSilence {
    Silence silence = 1;
    google.protobuf.Timestamp expires_at = 2;
}

message Matcher {
    enum Type {
        EQUAL = 0;
        REGEXP = 1;
        NOT_EQUAL = 2;
        NOT_REGEXP = 3;
    }
    Type type = 1;
    string name = 2;
    string pattern = 3;
}
```

## 11. API 연동

### 11.1 POST /api/v2/silences

```
postSilencesHandler(params):
    1. 요청 바디에서 Silence 모델 추출
    2. Matchers 파싱 및 유효성 검증
    3. Silences.Set(silence)
    4. 반환: {silenceID}
```

### 11.2 GET /api/v2/silences

```
getSilencesHandler(params):
    1. 필터 파라미터 파싱 (matchers)
    2. Silences.Query(QState(...))
    3. 결과를 API 모델로 변환
    4. 반환: [{silence}, ...]
```

### 11.3 DELETE /api/v2/silence/{id}

```
deleteSilenceHandler(params):
    1. Silence ID 추출
    2. Silences.Expire(id)
    3. 반환: 200 OK
```

## 12. 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `alertmanager_silences` | Gauge (state 레이블) | 상태별 Silence 수 |
| `alertmanager_silences_gc_duration_seconds` | Summary | GC 소요시간 |
| `alertmanager_silences_snapshot_duration_seconds` | Summary | 스냅샷 소요시간 |
| `alertmanager_silences_snapshot_size_bytes` | Gauge | 스냅샷 크기 |
| `alertmanager_silences_queries_total` | Counter | 쿼리 수 |
| `alertmanager_silences_query_errors_total` | Counter | 쿼리 오류 수 |
| `alertmanager_silences_query_duration_seconds` | Histogram | 쿼리 소요시간 |
| `alertmanager_silences_maintenance_total` | Counter | 유지보수 실행 수 |
| `alertmanager_silences_maintenance_errors_total` | Counter | 유지보수 오류 수 |

## 13. 실제 사용 시나리오

### 13.1 계획된 유지보수

```yaml
# amtool로 2시간 Silence 생성
amtool silence add \
  alertname=~".*" \
  instance="node-1" \
  --expires=2h \
  --comment="node-1 유지보수"
```

### 13.2 팀별 Silence

```yaml
# 특정 팀의 모든 경고 억제
amtool silence add \
  team="backend" \
  severity="warning" \
  --expires=1h \
  --comment="백엔드 팀 배포 중"
```

### 13.3 Silence의 영향 확인

```bash
# 현재 억제된 Alert 확인
amtool alert query --silenced

# 활성 Silence 확인
amtool silence query

# 특정 Silence의 매칭 Alert 확인
curl http://localhost:9093/api/v2/alerts?filter=alertname="HighCPU"&silenced=true
```
