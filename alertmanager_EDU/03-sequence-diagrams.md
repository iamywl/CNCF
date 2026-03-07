# Alertmanager 시퀀스 다이어그램

## 1. 개요

Alertmanager의 주요 유즈케이스별 요청 흐름을 시퀀스 다이어그램으로 정리한다.

## 2. Alert 수신 및 처리 흐름

### 2.1 Alert 수신 (API → Provider)

```mermaid
sequenceDiagram
    participant P as Prometheus
    participant API as API v2
    participant Prov as Provider (mem)
    participant Store as store.Alerts
    participant Marker as AlertMarker

    P->>API: POST /api/v2/alerts [{labels, annotations, startsAt, endsAt}]
    API->>API: postAlertsHandler() - 요청 파싱
    API->>API: 각 Alert 유효성 검증

    loop 각 Alert에 대해
        API->>Prov: Put([]*Alert)
        Prov->>Prov: PreStore 콜백 (검증, 제한)
        Prov->>Store: Set(alert)
        Store->>Store: Fingerprint 계산

        alt 기존 Alert 존재
            Store->>Store: 기존 Alert 업데이트 (Merge)
        else 새 Alert
            Store->>Store: map[fp] = alert
        end

        Prov->>Prov: PostStore 콜백
        Prov->>Prov: 리스너들에게 브로드캐스트
    end

    API-->>P: 200 OK
```

### 2.2 Dispatcher의 Alert 수집 및 라우팅

```mermaid
sequenceDiagram
    participant Prov as Provider
    participant Disp as Dispatcher
    participant Route as Route 트리
    participant AG as AggregationGroup

    Note over Disp: Run() 시작
    Disp->>Prov: SlurpAndSubscribe("dispatcher")
    Prov-->>Disp: 기존 Alert + Iterator

    loop Alert 수집 워커 (concurrency)
        Prov-->>Disp: 새 Alert 수신 (채널)
        Disp->>Disp: routeAlert(alert)
        Disp->>Route: Match(alert.Labels)

        alt 매칭 Route 있음
            Route-->>Disp: []*Route (매칭 결과)

            loop 각 매칭 Route에 대해
                Disp->>Disp: groupAlert(route, alert)
                Disp->>Disp: group_by 레이블로 그룹 키 생성

                alt 기존 Aggregation Group
                    Disp->>AG: insert(alert)
                else 새 Aggregation Group
                    Disp->>AG: 새 aggrGroup 생성
                    AG->>AG: GroupWait 타이머 시작
                end
            end
        else 매칭 없음
            Disp->>Disp: 루트 Route의 Receiver로 전달
        end
    end
```

### 2.3 Aggregation Group Flush 및 알림 전송

```mermaid
sequenceDiagram
    participant AG as AggregationGroup
    participant Pipeline as Notification Pipeline
    participant Mute as MuteStage
    participant Wait as WaitStage
    participant Dedup as DedupStage
    participant Retry as RetryStage
    participant Notifier as Integration
    participant NFLog as nflog

    Note over AG: GroupWait 타이머 만료
    AG->>AG: flush()
    AG->>AG: 현재 Alert 목록 수집
    AG->>Pipeline: Exec(ctx, alerts...)

    Pipeline->>Mute: Exec(ctx, alerts...)
    Mute->>Mute: Silence 확인
    Mute->>Mute: Inhibition 확인
    Note over Mute: 억제된 Alert 필터링
    Mute-->>Pipeline: 필터링된 alerts

    Pipeline->>Wait: Exec(ctx, alerts...)
    Note over Wait: RepeatInterval 확인
    Wait-->>Pipeline: alerts

    Pipeline->>Dedup: Exec(ctx, alerts...)
    Dedup->>NFLog: Query(receiver, groupKey)
    NFLog-->>Dedup: 이전 발송 기록
    Note over Dedup: 이미 발송된 Alert 필터링
    Dedup-->>Pipeline: 새로운 alerts

    Pipeline->>Retry: Exec(ctx, alerts...)

    loop 각 Integration에 대해
        Retry->>Notifier: Notify(ctx, alerts...)

        alt 성공
            Notifier-->>Retry: (false, nil)
        else 재시도 가능한 오류
            Notifier-->>Retry: (true, error)
            Note over Retry: Exponential Backoff 재시도
        else 영구 오류
            Notifier-->>Retry: (false, error)
            Note over Retry: 재시도 중단
        end
    end

    Pipeline->>NFLog: Log(receiver, groupKey, firingAlerts, resolvedAlerts)
    Note over NFLog: 발송 기록 저장
```

## 3. Silence 생성 흐름

```mermaid
sequenceDiagram
    participant User as 사용자
    participant API as API v2
    participant Sil as Silences
    participant Cluster as Cluster

    User->>API: POST /api/v2/silences {matchers, startsAt, endsAt, ...}
    API->>API: postSilencesHandler()
    API->>API: Matcher 파싱 및 유효성 검증

    API->>Sil: Set(silence)
    Sil->>Sil: ID 생성 (ULID)
    Sil->>Sil: 상태 저장 (st[id] = silence)
    Sil->>Sil: matcherIndex 업데이트
    Sil->>Sil: version 증가
    Sil->>Cluster: broadcast(silence 직렬화)
    Cluster-->>Cluster: Gossip으로 다른 인스턴스에 전파

    Sil-->>API: silenceID
    API-->>User: 200 OK {silenceID}
```

## 4. Silence 매칭 흐름

```mermaid
sequenceDiagram
    participant Pipeline as MuteStage
    participant Silencer as Silencer
    participant Cache as Silence Cache
    participant Sil as Silences
    participant Marker as AlertMarker

    Pipeline->>Silencer: Mutes(ctx, labelSet)
    Silencer->>Silencer: Fingerprint 계산

    Silencer->>Cache: 캐시 조회(fingerprint, version)

    alt 캐시 유효 (version 동일)
        Cache-->>Silencer: 이전 결과 반환
    else 캐시 미스 또는 무효
        Silencer->>Sil: Query(활성 Silence만)
        Sil-->>Silencer: []*Silence

        loop 각 Silence에 대해
            Silencer->>Silencer: Silence.Matchers ∩ Alert.Labels
            alt 매칭
                Silencer->>Silencer: silenceIDs에 추가
            end
        end

        Silencer->>Cache: 결과 캐시 저장
    end

    Silencer->>Marker: SetActiveOrSilenced(fp, silenceIDs)

    alt silenceIDs 있음
        Silencer-->>Pipeline: true (뮤트)
    else silenceIDs 없음
        Silencer-->>Pipeline: false (통과)
    end
```

## 5. Inhibition 흐름

```mermaid
sequenceDiagram
    participant Pipeline as MuteStage
    participant Inhibitor as Inhibitor
    participant Rules as InhibitRule[]
    participant SCache as Source Cache
    participant Marker as AlertMarker

    Note over Inhibitor: Run() - Alert 변경 감시 시작

    Pipeline->>Inhibitor: Mutes(ctx, targetLabels)

    loop 각 InhibitRule에 대해
        Inhibitor->>Rules: TargetMatchers.Matches(targetLabels)

        alt Target 매칭
            Inhibitor->>SCache: Source Alert 조회
            SCache-->>Inhibitor: 매칭되는 Source Alerts

            loop 각 Source Alert에 대해
                Inhibitor->>Inhibitor: Equal 레이블 비교
                Note over Inhibitor: Source와 Target의 Equal 레이블이 동일한지 확인

                alt Equal 레이블 일치
                    Inhibitor->>Marker: SetInhibited(targetFP, sourceAlertIDs...)
                    Inhibitor-->>Pipeline: true (억제)
                end
            end
        end
    end

    Inhibitor-->>Pipeline: false (통과)
```

## 6. 클러스터 상태 동기화 흐름

```mermaid
sequenceDiagram
    participant AM1 as Alertmanager 1
    participant ML1 as memberlist 1
    participant ML2 as memberlist 2
    participant AM2 as Alertmanager 2

    Note over AM1: Silence 생성
    AM1->>AM1: Silences.Set()
    AM1->>ML1: broadcast(silence 직렬화)

    Note over ML1,ML2: Gossip Protocol (UDP)
    ML1->>ML2: 메시지 전송
    ML2->>AM2: delegate.NotifyMsg(msg)
    AM2->>AM2: Silences.Merge(silence)

    Note over ML1,ML2: Push-Pull (TCP, 주기적)
    ML1->>ML2: 전체 상태 교환
    ML2->>AM2: delegate.GetBroadcasts()
    Note over AM2: 상태 병합 (CRDT 방식)

    Note over AM1: nflog 기록
    AM1->>AM1: nflog.Log()
    AM1->>ML1: broadcast(nflog 엔트리)
    ML1->>ML2: Gossip 전파
    ML2->>AM2: nflog.Merge(entry)
    Note over AM2: 중복 알림 방지에 활용
```

## 7. 설정 리로드 흐름

```mermaid
sequenceDiagram
    participant User as 운영자
    participant Main as main()
    participant Coord as Coordinator
    participant Disp as Dispatcher
    participant Inhib as Inhibitor
    participant API as API

    alt SIGHUP 시그널
        User->>Main: kill -HUP <pid>
    else HTTP 엔드포인트
        User->>Main: POST /-/reload
    end

    Main->>Coord: Reload()
    Coord->>Coord: LoadFile(configPath)
    Coord->>Coord: YAML 파싱 및 유효성 검증

    alt 설정 유효
        loop 각 구독자에 대해
            Coord->>Coord: subscriber(newConfig)
        end

        Coord->>Inhib: 기존 Inhibitor 중지
        Coord->>Inhib: 새 InhibitRule로 재생성
        Inhib->>Inhib: Run()

        Coord->>Disp: 기존 Dispatcher 중지
        Coord->>Disp: 새 Route 트리로 재생성
        Disp->>Disp: Run()

        Coord->>API: Update(config)

        Note over Coord: configHashMetric 업데이트
        Note over Coord: configSuccessMetric = 1
    else 설정 무효
        Coord->>Coord: 에러 로깅
        Note over Coord: configSuccessMetric = 0
    end
```

## 8. amtool CLI 흐름

### 8.1 Alert 조회

```
amtool alert query alertname="HighLatency"
    │
    ▼
GET /api/v2/alerts?filter=alertname%3D"HighLatency"
    │
    ▼
API.getAlertsHandler()
    ├→ Matcher 파싱
    ├→ Provider.GetPending() - 모든 Alert 조회
    ├→ alertFilter() - Matcher로 필터링
    ├→ Marker.Status() - 각 Alert 상태 확인
    └→ JSON 응답 반환
```

### 8.2 Silence 생성

```
amtool silence add alertname=Test_Alert
    │
    ▼
POST /api/v2/silences {matchers: [{name: "alertname", value: "Test_Alert", isEqual: true, isRegex: false}]}
    │
    ▼
API.postSilencesHandler()
    ├→ Silence 객체 생성
    ├→ Silences.Set(silence)
    └→ {silenceID} 반환
```

### 8.3 라우팅 테스트

```
amtool config routes test --config.file=config.yml severity=critical team=infra
    │
    ▼
Config 파일 로드
    │
    ▼
Route 트리 구축
    │
    ▼
Match(labels) - DFS 매칭
    │
    ▼
매칭된 Receiver 출력: "infra-pager"
```

## 9. Alert 생명주기 시퀀스

```
시간 ─────────────────────────────────────────────────→

[T0] Prometheus POST Alert (startsAt=T0)
      │
      ▼
[T0] Provider 저장
      │
      ▼
[T0] Dispatcher 수신 → Route 매칭 → AggregationGroup 생성
      │
      │  ◄── GroupWait (기본 30s) ──►
      │
[T0+30s] flush() → Notification Pipeline
      │  ├── MuteStage: Silence/Inhibition 확인
      │  ├── DedupStage: nflog 확인 (첫 알림이므로 통과)
      │  └── RetryStage: Integration 호출 (Slack, Email 등)
      │
      │  ◄── GroupInterval (기본 5m) ──►
      │
[T0+5m30s] 새 Alert 추가 시 flush()
      │  └── 그룹에 추가된 새 Alert만 전송
      │
      │  ◄── RepeatInterval (기본 4h) ──►
      │
[T0+4h] 변경 없어도 반복 전송
      │
      │
[Tn] Prometheus POST Alert (endsAt=Tn, resolved)
      │
      ▼
[Tn] Provider 업데이트
      │
      ▼
[Tn] Dispatcher flush() → resolved 알림 전송
      │
      │  ◄── GC Interval ──►
      │
[Tn+X] Provider GC → Alert 삭제, Marker 정리
```

## 10. 동시성 패턴 요약

```
┌─────────────────────────────────────────────┐
│              goroutine 구조                  │
│                                             │
│  [main]                                     │
│    ├── HTTP 서버                             │
│    ├── 시그널 핸들러                          │
│    │                                        │
│  [Dispatcher]                               │
│    ├── N개 Alert 수집 워커 (concurrency)     │
│    │   └── routeAlert() → groupAlert()      │
│    ├── AggregationGroup 유지보수 루프        │
│    │   └── 만료 그룹 정리                    │
│    └── 각 AggregationGroup                  │
│        └── flush 타이머 (GroupWait/Interval) │
│                                             │
│  [Inhibitor]                                │
│    └── Alert 변경 감시 루프                  │
│        └── Source Alert 캐시 업데이트         │
│                                             │
│  [Provider GC]                              │
│    └── 주기적 GC 루프                        │
│                                             │
│  [nflog Maintenance]                        │
│    └── GC + 스냅샷 저장                      │
│                                             │
│  [Silences Maintenance]                     │
│    └── GC + 스냅샷 저장                      │
│                                             │
│  [Cluster] (선택)                           │
│    └── memberlist goroutines                │
│        ├── Gossip 수신/송신                  │
│        ├── Push-Pull 주기적 교환             │
│        └── 피어 프로빙                       │
└─────────────────────────────────────────────┘
```
