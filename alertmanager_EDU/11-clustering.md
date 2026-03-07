# Alertmanager 클러스터링 Deep Dive

## 1. 개요

Alertmanager는 **hashicorp/memberlist** 라이브러리를 사용한 Gossip 프로토콜로 고가용성(HA) 클러스터를 구성한다. 클러스터를 통해 **Silence**와 **Notification Log(nflog)** 상태를 동기화하여 알림 중복 전송을 방지한다. `cluster/cluster.go`, `cluster/channel.go`, `cluster/delegate.go`에 구현되어 있다.

## 2. Peer 구조체

```go
// cluster/cluster.go
type Peer struct {
    mlist               *memberlist.Memberlist  // hashicorp/memberlist
    delegate            *delegate               // memberlist Delegate 구현
    resolvedPeers       []string               // 해석된 피어 주소
    resolvePeersTimeout time.Duration

    mtx    sync.RWMutex
    states map[string]State                     // 공유 상태: "nfl"→nflog, "sil"→silences

    stopc  chan struct{}                        // 종료 신호
    readyc chan struct{}                        // 준비 완료 신호

    peerLock    sync.RWMutex
    peers       map[string]peer                 // 피어 상태 추적
    failedPeers []peer                         // 실패한 피어 목록

    knownPeers     []string
    advertiseAddr  string

    logger *slog.Logger
    // Prometheus 메트릭...
}
```

### 2.1 peer 내부 타입

```go
// cluster/cluster.go
type peer struct {
    status    PeerStatus
    leaveTime time.Time
    *memberlist.Node
}

type PeerStatus int
const (
    StatusNone   PeerStatus = iota  // 알 수 없음
    StatusAlive                      // 활성
    StatusFailed                     // 실패
)
```

## 3. 인터페이스

### 3.1 ClusterPeer

```go
// cluster/cluster.go
type ClusterPeer interface {
    Name() string
    Status() string
    Peers() []ClusterMember
}
```

API에서 클러스터 상태를 조회할 때 사용한다.

### 3.2 ClusterChannel

```go
// cluster/cluster.go
type ClusterChannel interface {
    Broadcast([]byte)
}
```

상태 변경을 클러스터에 전파할 때 사용한다.

### 3.3 State 인터페이스

```go
// cluster/cluster.go
type State interface {
    MarshalBinary() ([]byte, error)       // 전체 상태 직렬화
    Merge(b []byte) error                 // 수신 데이터 병합
}
```

nflog과 Silences가 이 인터페이스를 구현한다.

## 4. 생성 (Create)

```go
// cluster/cluster.go
func Create(
    l *slog.Logger,
    reg prometheus.Registerer,
    bindAddr, advertiseAddr string,
    knownPeers []string,
    waitIfEmpty bool,
    pushPullInterval, gossipInterval, tcpTimeout,
    resolveTimeout, probeTimeout, probeInterval time.Duration,
    tlsTransportConfig *TLSTransportConfig,
    allowInsecureAdvertise bool,
    label, name string,
) (*Peer, error)
```

```
Create() 흐름:
    1. memberlist.Config 생성 (기본 LAN 설정)
    2. 커스텀 파라미터 적용:
       - GossipInterval, PushPullInterval
       - ProbeTimeout, ProbeInterval
       - TCPTimeout
    3. delegate 생성
    4. TLS 전송 설정 (선택)
    5. memberlist.Create(config)
    6. Peer 구조체 초기화
    7. readyc 채널 생성 (준비 신호)
```

## 5. Join() 피어 연결

```go
// cluster/cluster.go
func (p *Peer) Join(
    reconnectInterval time.Duration,  // 재연결 시도 간격 (기본 10s)
    reconnectTimeout time.Duration,   // 재연결 포기 시간 (기본 6h)
) error
```

```
Join() 흐름:
    1. knownPeers DNS 해석
    2. memberlist.Join(resolvedPeers)
    3. 재연결 goroutine 시작:
       - 주기적으로 실패한 피어에 재연결 시도
       - reconnectTimeout 지나면 포기
```

## 6. Settle() 안정화 대기

```go
// cluster/cluster.go
func (p *Peer) Settle(ctx context.Context, interval time.Duration)
```

```
Settle() 흐름:
    클러스터 멤버 수가 안정화될 때까지 대기:
    1. interval 간격으로 멤버 수 확인
    2. 연속 N번 동일하면 안정화 판단
    3. close(p.readyc) → WaitReady() 해제
```

GossipSettleStage에서 `p.WaitReady(ctx)`를 호출하여 Pipeline이 안정화 전에 알림을 보내지 않도록 한다.

## 7. AddState() 상태 등록

```go
// cluster/cluster.go
func (p *Peer) AddState(name string, s State, reg prometheus.Registerer) ClusterChannel
```

```
AddState("nfl", nflog, reg):
    1. p.states["nfl"] = nflog
    2. ClusterChannel 반환 (Broadcast 함수 포함)
    3. nflog.SetBroadcast(channel.Broadcast)
```

nflog과 Silences가 각각 "nfl"과 "sil"로 등록된다.

## 8. delegate (memberlist.Delegate 구현)

### 8.1 구조체

```go
// cluster/delegate.go
type delegate struct {
    *Peer
    bcast *memberlist.TransmitLimitedQueue  // 브로드캐스트 큐
}
```

### 8.2 핵심 메서드

| 메서드 | 호출 시점 | 역할 |
|--------|----------|------|
| `NotifyMsg(msg []byte)` | Gossip 메시지 수신 | State.Merge() 호출 |
| `GetBroadcasts(overhead, limit int)` | Gossip 메시지 송신 | 대기 중인 브로드캐스트 반환 |
| `LocalState(join bool)` | Push-Pull 교환 | 전체 상태 직렬화 반환 |
| `MergeRemoteState(buf []byte, join bool)` | Push-Pull 수신 | State.Merge() 호출 |
| `NodeMeta(limit int)` | 노드 메타데이터 요청 | 노드 정보 반환 |
| `NotifyJoin(node)` | 새 노드 합류 | peers 맵 업데이트 |
| `NotifyLeave(node)` | 노드 이탈 | peers 맵 업데이트 |
| `NotifyUpdate(node)` | 노드 업데이트 | peers 맵 업데이트 |

### 8.3 NotifyMsg 흐름

```
다른 인스턴스에서 Gossip 메시지 수신
    │
    ▼
delegate.NotifyMsg(msg)
    │
    ├─ 메시지에서 state 이름 추출 ("nfl" 또는 "sil")
    │
    ├─ p.states[name].Merge(data)
    │   ├─ nflog: 발송 기록 병합
    │   └─ silences: Silence 상태 병합
    │
    └─ 메트릭 업데이트
```

## 9. Gossip vs Push-Pull

```
┌─────────────────────────────────────────────────────┐
│                                                      │
│  Gossip (UDP, 200ms 간격)                            │
│  ─────────────────────                               │
│  - 작은 메시지 (개별 변경사항)                         │
│  - 빠른 전파, 낮은 대역폭                             │
│  - NotifyMsg() → Merge()                             │
│  - 새 Silence, 새 nflog 엔트리                       │
│                                                      │
│  Push-Pull (TCP, 1분 간격)                           │
│  ───────────────────────                             │
│  - 전체 상태 교환                                     │
│  - 높은 대역폭, 완전한 동기화                          │
│  - LocalState() ↔ MergeRemoteState()                 │
│  - 누락된 데이터 복구                                 │
│                                                      │
│  ┌──────┐  Gossip(UDP)  ┌──────┐                    │
│  │ AM 1 │◄═════════════►│ AM 2 │                    │
│  │      │  Push-Pull    │      │                    │
│  │      │◄─────────────►│      │                    │
│  └──────┘  (TCP)        └──────┘                    │
│                                                      │
└─────────────────────────────────────────────────────┘
```

## 10. Broadcast 메커니즘

```go
// cluster/channel.go
type Channel struct {
    key   string
    send  func([]byte)
    peers func() []*memberlist.Node
}

func (c *Channel) Broadcast(b []byte) {
    // TransmitLimitedQueue에 메시지 추가
    // memberlist가 Gossip 라운드에서 자동 전송
}
```

```
Silence.Set() 또는 nflog.Log():
    1. 데이터 Protobuf 직렬화
    2. Channel.Broadcast(bytes)
    3. TransmitLimitedQueue에 추가
    4. 다음 Gossip 라운드에서 전송
       └─ delegate.GetBroadcasts() → 큐에서 메시지 반환
```

## 11. TLS 전송

```go
// cluster/tls_transport.go
type TLSTransport struct {
    bindAddr  string
    tlsConfig *tls.Config
    // ...
}
```

클러스터 간 통신을 TLS로 암호화할 수 있다:

```bash
alertmanager \
  --cluster.tls-config=cluster-tls.yml
```

```yaml
# cluster-tls.yml
tls_server_config:
  cert_file: /path/to/cert.pem
  key_file: /path/to/key.pem
  client_auth_type: RequireAndVerifyClientCert
  client_ca_file: /path/to/ca.pem
tls_client_config:
  cert_file: /path/to/cert.pem
  key_file: /path/to/key.pem
  ca_file: /path/to/ca.pem
```

## 12. nflog의 State 구현

```go
// nflog/nflog.go
func (l *Log) MarshalBinary() ([]byte, error) {
    // 전체 state를 Protobuf로 직렬화
}

func (l *Log) Merge(b []byte) error {
    // 수신 데이터를 디시리얼라이즈
    // 각 엔트리에 대해:
    //   state.merge(entry, now) → timestamp 기반 병합
}
```

## 13. Silences의 State 구현

```go
// silence/silence.go
func (s *Silences) MarshalBinary() ([]byte, error) {
    // 전체 st를 Protobuf로 직렬화
}

func (s *Silences) Merge(b []byte) error {
    // 수신 데이터를 디시리얼라이즈
    // 각 Silence에 대해:
    //   timestamp 기반 병합
    //   matcherIndex 업데이트
    //   version++
}
```

## 14. 클러스터 라벨

```bash
alertmanager --cluster.label="team-backend"
```

클러스터 라벨은 Gossip 패킷에 포함되어, 동일 네트워크에서 여러 클러스터를 격리한다. 라벨이 다른 피어의 메시지는 무시된다.

## 15. 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `alertmanager_cluster_enabled` | Gauge | 클러스터 활성화 여부 |
| `alertmanager_cluster_members` | Gauge | 클러스터 멤버 수 |
| `alertmanager_cluster_health_score` | Gauge | 건강 점수 (0=최상) |
| `alertmanager_cluster_peer_info` | Gauge | 피어 정보 |
| `alertmanager_cluster_messages_received_total` | Counter | 수신 메시지 수 |
| `alertmanager_cluster_messages_received_size_total` | Counter | 수신 메시지 총 크기 |
| `alertmanager_cluster_messages_sent_total` | Counter | 송신 메시지 수 |
| `alertmanager_cluster_messages_sent_size_total` | Counter | 송신 메시지 총 크기 |
| `alertmanager_cluster_messages_publish_total` | Counter | 발행 메시지 수 |
| `alertmanager_cluster_messages_pruned_total` | Counter | 제거된 메시지 수 |

## 16. HA 동작 시나리오

```
시나리오: 3-노드 클러스터에서 Silence 생성

1. 사용자가 AM-1에 Silence 생성 요청
   AM-1: Silences.Set(silence) → st["sil-001"] = silence
         → version++ → broadcast(silence)

2. Gossip 전파 (200ms 이내)
   AM-1 → AM-2: delegate.NotifyMsg() → Silences.Merge(silence)
   AM-1 → AM-3: delegate.NotifyMsg() → Silences.Merge(silence)

3. Push-Pull 확인 (1분 이내)
   AM-1 ↔ AM-2: 전체 상태 교환 → 누락 확인
   AM-2 ↔ AM-3: 전체 상태 교환 → 누락 확인

4. 결과: 3개 인스턴스 모두 동일한 Silence 보유
   → 어느 인스턴스에서든 해당 Alert 억제
```

```
시나리오: 알림 중복 방지

1. Prometheus가 AM-1, AM-2, AM-3 모두에 Alert 전송

2. AM-1이 먼저 flush → 알림 전송 → nflog 기록
   → broadcast(nflog 엔트리)

3. AM-2, AM-3이 nflog 수신 → DedupStage에서 확인
   → 이미 전송됨 → 알림 생략

4. 결과: 3개 인스턴스 중 1개만 알림 전송
```

## 17. 클러스터 비활성화

```bash
alertmanager --cluster.listen-address=""
```

빈 문자열로 설정하면 클러스터 기능이 비활성화된다. 단일 인스턴스로 운영 시 사용한다.

## 18. 네트워크 요구사항

```
┌──────────────────────────────────┐
│  포트 9094                       │
│                                  │
│  UDP: Gossip 메시지              │
│    - 작은 패킷 (수백 바이트)      │
│    - 200ms 간격                  │
│    - 빠른 전파                   │
│                                  │
│  TCP: Push-Pull + 상태 교환      │
│    - 큰 데이터 (전체 상태)        │
│    - 1분 간격                    │
│    - 완전한 동기화               │
│                                  │
│  방화벽: UDP + TCP 모두 허용 필수│
│  컨테이너: 두 프로토콜 모두 노출  │
└──────────────────────────────────┘
```
