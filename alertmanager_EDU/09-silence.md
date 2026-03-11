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

## 14. matcherIndex 소스 코드 상세

### 14.1 matcherIndex 타입

```go
// silence/silence.go
type matcherIndex map[string]labels.MatcherSet
```

키는 Silence ID(ULID), 값은 컴파일된 MatcherSet이다.

### 14.2 add 메서드

```go
// silence/silence.go
func (c matcherIndex) add(s *pb.Silence) (labels.MatcherSet, error) {
    matcherSet := make(labels.MatcherSet, 0, len(s.MatcherSets))

    for _, ms := range s.MatcherSets {
        matchers := make(labels.Matchers, len(ms.Matchers))
        for i, m := range ms.Matchers {
            var mt labels.MatchType
            switch m.Type {
            case pb.Matcher_EQUAL:
                mt = labels.MatchEqual
            case pb.Matcher_NOT_EQUAL:
                mt = labels.MatchNotEqual
            case pb.Matcher_REGEXP:
                mt = labels.MatchRegexp
            case pb.Matcher_NOT_REGEXP:
                mt = labels.MatchNotRegexp
            default:
                return nil, fmt.Errorf("unknown matcher type %q", m.Type)
            }
            matcher, err := labels.NewMatcher(mt, m.Name, m.Pattern)
            if err != nil {
                return nil, err
            }
            matchers[i] = matcher
        }
        matcherSet = append(matcherSet, &matchers)
    }

    c[s.Id] = matcherSet
    return matcherSet, nil
}
```

**왜 Protobuf Matcher를 labels.Matcher로 변환하는가?** Protobuf 메시지는 직렬화/역직렬화에 적합하지만, 매칭 로직(특히 정규식 컴파일)은 `labels.Matcher`에 구현되어 있다. `add()`에서 정규식이 컴파일되어 캐시되므로, 매칭 시마다 재컴파일하지 않는다.

### 14.3 get 메서드

```go
func (c matcherIndex) get(s *pb.Silence) (labels.MatcherSet, error) {
    if m, ok := c[s.Id]; ok {
        return m, nil
    }
    return nil, ErrNotFound
}
```

## 15. OpenTelemetry 추적

```go
// silence/silence.go
var tracer = otel.Tracer("github.com/prometheus/alertmanager/silence")
```

Silence 저장소는 OpenTelemetry 추적을 지원한다. `Set()`, `Expire()`, `Query()` 등 주요 메서드에서 span을 생성하여 분산 추적을 제공한다:

```
Silence 추적 계층:

API Request (HTTP span)
  └─ Silences.Set() (silence span)
       ├─ attribute: silence.id
       ├─ attribute: silence.matchers
       └─ broadcast → Cluster Channel
            └─ delegate.NotifyMsg()
                 └─ Silences.Merge()
```

## 16. 에러 타입과 처리

```go
// silence/silence.go
var ErrNotFound = errors.New("silence not found")
var ErrInvalidState = errors.New("invalid state")
```

### 16.1 Set() 에러 시나리오

```
Set() 에러 분류:

1. 유효성 검증 실패
   ├─ Matchers가 0개 → "at least one matcher required"
   ├─ StartsAt >= EndsAt → "end time must be after start time"
   └─ Matcher Name/Value 유효하지 않음

2. Limits 초과
   ├─ MaxSilences 초과 → "too many silences"
   └─ MaxSilenceSizeBytes 초과 → "silence too large"

3. 업데이트 시
   ├─ 기존 Silence 미발견 → ErrNotFound
   └─ 이미 만료된 Silence 수정 시도 → ErrInvalidState

4. 내부 오류
   └─ matcherIndex.add() 실패 → 정규식 컴파일 에러
```

### 16.2 Expire() 에러 시나리오

```
Expire() 에러 분류:
  ├─ Silence 미발견 → ErrNotFound
  ├─ 이미 만료됨 → 건너뜀 (에러가 아님)
  └─ 직렬화 실패 → Protobuf 에러
```

## 17. 클러스터 Merge 상세

### 17.1 CRDT Merge 규칙

```
Silences.Merge(entries):
    각 entry에 대해:

    1. 기존 st에 해당 ID 없음
       → 새로 추가
       → matcherIndex.add()
       → version++

    2. 기존 st에 해당 ID 있음
       → 수신 entry의 UpdatedAt > 기존 UpdatedAt
          → 수신 entry로 교체
          → matcherIndex 업데이트
          → version++
       → 수신 entry의 UpdatedAt <= 기존 UpdatedAt
          → 무시 (기존이 더 최신)

    CRDT 속성:
    - 교환 법칙: Merge(A, B) == Merge(B, A)
    - 결합 법칙: Merge(Merge(A, B), C) == Merge(A, Merge(B, C))
    - 멱등성: Merge(A, A) == A
```

### 17.2 version과 캐시 무효화

```
Set/Expire/Merge 시:
    version++
         ↓
    다음 Silencer.Mutes() 호출:
         ↓
    cache.Get(fp, currentVersion)
         ↓
    cacheEntry.version != currentVersion
         ↓
    캐시 미스 → 전체 매칭 재수행
         ↓
    cache.Set(fp, currentVersion, silenceIDs)
```

**왜 글로벌 version을 사용하는가?** 개별 Silence의 변경을 추적하는 대신, 전체 상태의 version을 하나 증가시킨다. 이 방식은 단순하지만, 하나의 Silence만 변경되어도 모든 캐시가 점진적으로 재계산된다. 그러나 lazy invalidation으로 인해 실제 Mutes() 호출이 있는 Alert에 대해서만 재계산되므로, 실전에서는 효율적이다.

## 18. 성능 고려사항

### 18.1 매칭 성능

```
Silencer.Mutes() 성능:

최선: O(1) — 캐시 히트
  → fp로 cacheEntry 조회 → version 일치 → 즉시 반환

최악: O(S × M) — 캐시 미스
  → S = 활성 Silence 수
  → M = 평균 Matcher 수
  → 모든 활성 Silence와 Alert Labels 매칭

캐시 히트율 최적화:
  - Silence 변경이 드물면 version이 안정 → 높은 히트율
  - Alert 수가 많으면 캐시 메모리 사용량 증가
```

### 18.2 GC 성능

```
GC() 비용: O(N) where N = 전체 Silence 수
  - 만료 + retention 경과 확인
  - 삭제된 Silence의 matcherIndex 엔트리 제거
  - version 증가

Maintenance 주기 (기본 15분):
  - GC + Snapshot을 순차 실행
  - 대규모 Silence 수(수천 개)에서는 Snapshot I/O가 병목
```

### 18.3 스냅샷 크기 관리

```
스냅샷 크기 = Σ(각 Silence의 Protobuf 직렬화 크기)

크기 줄이기:
  - retention을 짧게 설정 → 만료된 Silence 빠르게 삭제
  - 불필요한 Silence 정리 (amtool silence expire --all)
  - MaxSilences 제한으로 폭발적 증가 방지
```

## 19. 테스트 전략

### 19.1 quartz.Clock을 이용한 시간 제어

```go
// silence/silence.go
type Silences struct {
    clock quartz.Clock   // 테스트에서 모의 시계 주입
    // ...
}
```

**왜 quartz.Clock을 사용하는가?** Silence의 활성/만료 상태는 시간에 의존한다. `time.Now()`를 직접 사용하면 테스트가 비결정적이 된다. `quartz.Clock`으로 시간을 제어하여:
- 특정 시점에 Silence가 활성화되는지 검증
- GC에서 retention 경과 후 삭제되는지 검증
- 만료 시점 정확성 검증

### 19.2 핵심 테스트 시나리오

```
1. Set + Query 왕복 테스트
   - 생성 후 Query로 조회 → 일치 확인

2. Expire 테스트
   - Set → Expire → Query(QState("expired")) → 만료 확인

3. GC 테스트
   - Set → Expire → 시간 경과(retention) → GC() → 삭제 확인

4. Merge 테스트
   - 두 인스턴스에서 독립적으로 Silence 생성
   - Merge 후 양쪽 모두 동일한 상태

5. 캐시 테스트
   - Mutes() → 캐시 히트 확인
   - Set() → version++ → Mutes() → 캐시 미스 확인

6. Limits 테스트
   - MaxSilences 초과 시 에러 반환
   - MaxSilenceSizeBytes 초과 시 에러 반환
```
