# PoC-04: Watch 메커니즘

## 핵심 개념

etcd의 Watch는 키의 변경 이벤트를 실시간으로 감시하는 메커니즘이다. 핵심 설계는 **synced/unsynced 워처 그룹 분리**로, 새 이벤트의 즉시 전달과 과거 이벤트의 비동기 따라잡기를 효율적으로 처리한다.

### synced vs unsynced 워처

```
Watch 등록 시:
  if startRev > currentRev || startRev == 0:
      → synced 그룹 (최신 이벤트 즉시 수신)
  else:
      → unsynced 그룹 (과거 이벤트 따라잡기 필요)
```

| 그룹 | 조건 | 동작 |
|------|------|------|
| synced | startRev >= currentRev 또는 0 | notify()로 즉시 이벤트 수신 |
| unsynced | startRev < currentRev | syncWatchers()로 과거 이벤트 전달 후 synced로 이동 |

### watchableStore 구조

```
watchableStore {
    *store              ← 기반 MVCC 저장소
    synced   watcherGroup   ← 동기화된 워처들
    unsynced watcherGroup   ← 과거 이벤트 따라잡기 중인 워처들
    victims  []watcherBatch ← 채널 블로킹된 워처들
}
```

### 이벤트 전달 흐름

```
새 쓰기 발생 (Put/Delete)
    │
    ├──→ notify() ──→ synced 워처에 즉시 전달
    │                  └──→ 채널 가득 차면 → victim 처리
    │
    └──→ 이벤트 저장 (이후 unsynced 워처가 따라잡기용)

백그라운드 (100ms 주기):
    syncWatchers()
    ├── unsynced 워처의 최소 minRev 계산
    ├── minRev ~ currentRev 범위의 이벤트 조회
    ├── 각 워처에 해당 이벤트 배치 전달
    └── 완료된 워처를 synced로 이동
```

## 구현 설명

### Watch 등록

```go
synced := startRev == 0 || startRev >= s.currentRev
if synced {
    w.minRev = s.currentRev
    s.synced.add(w)
} else {
    s.unsynced.add(w)
}
```

### notify (synced 워처 알림)

새 쓰기(Put/Delete) 시 호출. synced 그룹에서 해당 키를 감시하는 워처를 찾아 이벤트를 채널로 전달한다.

### syncWatchers (unsynced 워처 동기화)

1. unsynced 워처들 중 최소 `minRev` 계산
2. `minRev` ~ `currentRev` 범위의 이벤트 조회
3. 각 워처에 해당하는 이벤트를 배치로 전달
4. 완료된 워처를 synced 그룹으로 이동

### WatcherGroup

키별 워처 집합을 관리. 실제 etcd는 단일 키 워처(`keyWatchers`)와 범위 워처(`ranges`, IntervalTree)를 구분하지만, 이 PoC에서는 단일 키 워처만 구현한다.

## 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `server/storage/mvcc/watchable_store.go` | watchableStore, watcher, notify, syncWatchers |
| `server/storage/mvcc/watcher_group.go` | watcherGroup, watcherBatch, eventBatch |
| `api/v3/mvccpb/kv.proto` | Event, EventType 정의 |

## 실행 방법

```bash
go run main.go
```

## 예상 출력

```
=== etcd Watch 메커니즘 시뮬레이션 ===

--- 시나리오 1: 사전 데이터 + synced Watch ---
  사전 데이터: rev=1, 2, 3

Watch 등록 (startRev=0, 최신부터):
  [Watch #1] 키="config/db_host" startRev=0 → synced 그룹 (minRev=4)

새 이벤트 발생:
  Watch #1 수신: [PUT] key="config/db_host" value="192.168.1.1" rev=4

--- 시나리오 2: 과거 리비전 Watch (unsynced → synced) ---
Watch 등록 (startRev=1, 과거부터):
  [Watch #2] 키="config/db_host" startRev=1 → unsynced 그룹 (과거 이벤트 따라잡기 필요)

syncWatchers가 과거 이벤트를 전달하는 중...
  [syncWatchers] 1개 워처를 unsynced → synced로 이동
Watch #2 수신 (과거 이벤트):
  WatchID=2, rev=4: [PUT] key="config/db_host" value="localhost" rev=1, ...

synced 상태에서 새 이벤트:
  Watch #2 수신: [PUT] key="config/db_host" value="10.0.0.99" rev=6

--- 시나리오 3: 다중 키 Watch ---
server/addr Watch 수신:
  WatchID=3, rev=7: [PUT] key="server/addr" value="10.0.0.1" rev=7
  WatchID=3, rev=10: [PUT] key="server/addr" value="10.0.0.2" rev=10
server/port Watch 수신:
  WatchID=4, rev=8: [PUT] key="server/port" value="8080" rev=8

--- 시나리오 4: DELETE 이벤트 ---
DELETE 이벤트:
  WatchID=5, rev=12: [DELETE] key="temp/key" rev=12

--- 시나리오 5: synced/unsynced 상태 ---
  synced 워처 수: 5
  unsynced 워처 수: 0
  현재 리비전: 13
  전체 이벤트 수: 12

=== Watch 메커니즘 핵심 원리 ===
1. synced 워처: currentRev과 동기화됨 → notify()로 즉시 이벤트 수신
2. unsynced 워처: startRev < currentRev → 과거 이벤트를 따라잡아야 함
3. syncWatchers(): 주기적으로 unsynced 워처의 과거 이벤트 전달
4. 동기화 완료 후 unsynced → synced 이동
5. 채널이 가득 차면 victim 처리 (이 PoC에서는 스킵)
```
