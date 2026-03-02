# 02. 시스템 아키텍처 (Architecture & Design)

## 전체 시스템 아키텍처

Hubble은 크게 **4개 계층**으로 구성됩니다: CLI 클라이언트, Relay, Server, Cilium 데이터플레인.

```mermaid
graph TB
    subgraph "사용자 영역"
        CLI["Hubble CLI<br/>(hubble observe/status/list)"]
        UI["Hubble UI<br/>(웹 대시보드, Beta)"]
    end

    subgraph "컨트롤 플레인"
        RELAY["Hubble Relay<br/>(멀티 노드 통합)"]
    end

    subgraph "노드 1"
        SERVER1["Hubble Server<br/>(:4245 gRPC)"]
        AGENT1["Cilium Agent"]
        BPF1["eBPF Datapath"]
    end

    subgraph "노드 2"
        SERVER2["Hubble Server<br/>(:4245 gRPC)"]
        AGENT2["Cilium Agent"]
        BPF2["eBPF Datapath"]
    end

    subgraph "Kubernetes"
        K8S["kube-apiserver"]
    end

    CLI -->|"gRPC/TLS"| RELAY
    CLI -->|"gRPC (직접 연결)"| SERVER1
    CLI -->|"kubectl port-forward"| SERVER1
    UI -->|"gRPC-Web"| RELAY

    RELAY -->|"gRPC/TLS (내부)"| SERVER1
    RELAY -->|"gRPC/TLS (내부)"| SERVER2

    SERVER1 --- AGENT1
    SERVER2 --- AGENT2

    AGENT1 -->|"perf ring buffer"| BPF1
    AGENT2 -->|"perf ring buffer"| BPF2

    AGENT1 -->|"Pod/Service 메타데이터"| K8S
    AGENT2 -->|"Pod/Service 메타데이터"| K8S
```

### 왜 이 구조인가?

1. **Server가 각 노드에 내장**: eBPF perf ring buffer는 노드 로컬이므로, 데이터 원천에 가장 가까운 곳에서 파싱/필터링하여 네트워크 오버헤드를 최소화
2. **Relay로 통합**: 클라이언트가 각 노드에 직접 연결할 필요 없이, 단일 진입점에서 클러스터 전체 이벤트를 시간순으로 병합
3. **CLI가 독립 바이너리**: Server 의존성 없이 빌드/배포 가능, 다양한 OS에서 사용 가능

---

## 컴포넌트 상세

### Hubble CLI (이 저장소)

```mermaid
graph LR
    subgraph "CLI 내부 구조"
        COBRA["Cobra<br/>(커맨드 파싱)"]
        VIPER["Viper<br/>(설정 관리)"]
        CONN["conn 패키지<br/>(gRPC 연결)"]
        PRINTER["printer 패키지<br/>(출력 포맷팅)"]
    end

    USER["사용자 입력"] --> COBRA
    COBRA --> VIPER
    COBRA --> CONN
    CONN -->|"gRPC Stream"| GRPC["Hubble Server/Relay"]
    GRPC -->|"Flow 데이터"| PRINTER
    PRINTER --> OUTPUT["터미널 출력<br/>(compact/json/table/dict)"]
```

**설계 결정:**
- **Cobra + Viper 조합**: 플래그, 환경변수, 설정파일을 통합 관리. `HUBBLE_` 접두어로 환경변수 자동 매핑
- **gRPC 스트리밍**: `observe --follow` 시 서버 푸시 방식으로 실시간 플로우 수신. HTTP polling 대비 지연 최소화
- **출력 포맷 분리**: Printer 패키지가 데이터 변환과 표시를 담당하여, 새로운 출력 형식 추가가 용이

### Hubble Server

```mermaid
graph TB
    subgraph "Hubble Server (노드 내)"
        GRPC_SVC["gRPC Services<br/>(Observer, Peer, Health)"]
        OBSERVER["LocalObserverServer"]
        RING["Ring Buffer<br/>(순환 버퍼)"]
        PARSER["Parser<br/>(Decoder)"]
        HOOKS["Hook System<br/>(7개 확장 포인트)"]
        FILTERS["Filter Engine<br/>(50+ 필터)"]
        METRICS["Metrics<br/>(Prometheus)"]
        NS_MGR["Namespace Manager"]
    end

    CLIENT["CLI/Relay"] -->|"GetFlows RPC"| GRPC_SVC
    GRPC_SVC --> OBSERVER
    OBSERVER --> RING
    OBSERVER --> PARSER
    OBSERVER --> HOOKS
    OBSERVER --> FILTERS
    HOOKS --> METRICS

    BPF["eBPF Datapath"] -->|"perf ring buffer"| EVENTS["이벤트 채널"]
    EVENTS --> OBSERVER

    K8S["Kubernetes API"] -->|"Endpoint/Identity"| GETTERS["Getter 인터페이스<br/>(DNS, Endpoint, Identity,<br/>IP, Service, Link)"]
    GETTERS --> PARSER
```

**설계 결정:**
- **Ring Buffer**: 메모리 효율적인 순환 버퍼로 최근 N개 플로우만 유지. 메모리 사용량 예측 가능하고 GC 압력 최소화
- **Hook System**: 메트릭, 필터 등을 플러그인 방식으로 확장. 각 Hook은 `(stop bool, error)` 반환하여 체인 중단 가능
- **Getter 인터페이스**: 파서가 쿠버네티스 메타데이터를 직접 조회하지 않고 인터페이스를 통해 접근. 테스트 용이성과 관심사 분리

### Hubble Relay

```mermaid
graph TB
    CLIENT["CLI / UI"] -->|"GetFlows"| RELAY["Hubble Relay"]

    RELAY -->|"peer pool"| PEER_MGR["Peer Manager"]
    PEER_MGR -->|"gRPC"| NODE1["Node 1 Server"]
    PEER_MGR -->|"gRPC"| NODE2["Node 2 Server"]
    PEER_MGR -->|"gRPC"| NODE3["Node 3 Server"]

    NODE1 -->|"Flow stream"| MERGER["Priority Queue<br/>(타임스탬프 기준 정렬)"]
    NODE2 -->|"Flow stream"| MERGER
    NODE3 -->|"Flow stream"| MERGER

    MERGER -->|"통합 스트림"| CLIENT
```

**설계 결정:**
- **Priority Queue로 병합**: 각 노드에서 독립적으로 도착하는 플로우를 타임스탬프 기준으로 정렬하여 시간순 통합 스트림 제공
- **NodeStatusEvent 전파**: 노드 연결 상태(CONNECTED, UNAVAILABLE, GONE, ERROR)를 클라이언트에 실시간 전달

---

## 통신 프로토콜

```mermaid
graph LR
    subgraph "프로토콜 스택"
        L1["CLI ↔ Server/Relay"]
        L2["Server ↔ Cilium Agent"]
        L3["Agent ↔ Kernel"]
    end

    L1 --- P1["gRPC over TLS<br/>(HTTP/2, Protobuf)"]
    L2 --- P2["In-process<br/>(같은 바이너리)"]
    L3 --- P3["BPF perf ring buffer<br/>(공유 메모리)"]
```

| 구간 | 프로토콜 | 인증 | 이유 |
|------|---------|------|------|
| CLI → Server | gRPC/TLS | TLS 인증서 or Basic Auth | 보안 통신, 양방향 스트리밍 필요 |
| CLI → Server (개발) | gRPC (평문) | 없음 | kubectl port-forward 시 로컬 통신 |
| Server ↔ Agent | In-process | 해당 없음 | Server가 Cilium Agent 프로세스 내에 내장 |
| Agent ↔ Kernel | perf ring buffer | 해당 없음 | 최소 오버헤드의 커널-유저 공간 데이터 전달 |
| Relay → Servers | gRPC/TLS (내부) | mTLS | 클러스터 내부 노드 간 보안 통신 |

---

## 데이터 흐름 개요

```mermaid
graph LR
    PACKET["네트워크 패킷"] -->|"1. eBPF 캡처"| BPF["BPF 프로그램"]
    BPF -->|"2. perf event"| RING_BUF["Perf Ring Buffer"]
    RING_BUF -->|"3. 읽기"| MONITOR["Monitor Event"]
    MONITOR -->|"4. 디코딩"| FLOW["Flow (Protobuf)"]
    FLOW -->|"5. 필터링"| FILTERED["필터 통과 Flow"]
    FILTERED -->|"6. 저장"| STORAGE["Ring Buffer"]
    FILTERED -->|"7. 메트릭"| PROM["Prometheus"]
    STORAGE -->|"8. gRPC 스트림"| CLIENT["CLI 출력"]
```

1. **eBPF 캡처**: 커널의 네트워크 경로에 삽입된 BPF 프로그램이 패킷 이벤트를 생성
2. **perf event**: BPF 프로그램이 perf ring buffer에 이벤트 기록 (최소 오버헤드)
3. **Monitor 읽기**: Cilium Agent가 perf ring buffer에서 raw 바이트를 읽음
4. **디코딩**: Parser가 raw 바이트를 Flow protobuf 메시지로 변환 (L3/L4/L7 파싱 + K8s 메타데이터 enrichment)
5. **필터링**: whitelist/blacklist 필터 적용
6. **저장**: 순환 버퍼에 저장 (고정 크기, FIFO)
7. **메트릭**: Hook을 통해 Prometheus 카운터/히스토그램 업데이트
8. **전달**: gRPC 스트리밍으로 클라이언트에 전달

---

## 배포 토폴로지

### 단일 노드 모드

```
┌─────────────────────────────┐
│  hubble observe --server    │
│  localhost:4245              │
└──────────┬──────────────────┘
           │ kubectl port-forward
           ↓
┌─────────────────────────────┐
│  Cilium Agent + Hubble      │
│  (단일 노드)                │
└─────────────────────────────┘
```

- 디버깅/개발 시 사용
- `kubectl port-forward`로 로컬 포트를 Hubble에 연결

### 멀티 노드 모드 (Relay)

```
┌──────────────────────┐
│  hubble observe       │
│  --server relay:4245  │
└──────────┬───────────┘
           ↓
┌──────────────────────┐
│  Hubble Relay        │
│  (Deployment)        │
└──┬───────┬───────┬───┘
   ↓       ↓       ↓
┌──────┐┌──────┐┌──────┐
│Node 1││Node 2││Node 3│
│Hubble││Hubble││Hubble│
└──────┘└──────┘└──────┘
```

- 프로덕션 환경 표준 구성
- Relay가 클러스터 전체 이벤트를 통합

---

## 직접 실행해보기 (PoC)

| PoC | 실행 | 학습 내용 |
|-----|------|----------|
| [poc-grpc-streaming](poc-grpc-streaming/) | `cd poc-grpc-streaming && go run main.go` | Server Streaming 패턴 (GetFlows RPC) |
| [poc-relay-merge](poc-relay-merge/) | `cd poc-relay-merge && go run main.go` | Priority Queue로 멀티 노드 Flow 병합 |
| [poc-tls-auth](poc-tls-auth/) | `cd poc-tls-auth && go run main.go` | mTLS/TLS 인증, 인증서 체인, TLS 1.3 강제 |
| [poc-peer-discovery](poc-peer-discovery/) | `cd poc-peer-discovery && go run main.go` | Peer 디스커버리, 연결 상태, 지수 백오프 |
| [poc-grpc-interceptor](poc-grpc-interceptor/) | `cd poc-grpc-interceptor && go run main.go` | gRPC Interceptor 체이닝 (메트릭, 인증, 버전) |
