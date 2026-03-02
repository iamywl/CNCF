# 04. 시퀀스 다이어그램 (Sequence Diagrams)

## 1. `hubble observe` 커맨드 전체 흐름

가장 핵심적인 기능인 Flow 관찰의 전체 시퀀스입니다.

```mermaid
sequenceDiagram
    actor User as 사용자
    participant CLI as Hubble CLI
    participant Cobra as Cobra/Viper
    participant Conn as conn 패키지
    participant Server as Hubble Server (gRPC)
    participant Observer as LocalObserverServer
    participant Parser as Parser (Decoder)
    participant Ring as Ring Buffer
    participant Filter as Filter Engine
    participant Printer as Printer
    participant BPF as eBPF Datapath

    User->>CLI: hubble observe --follow -t drop

    Note over CLI, Cobra: 1단계: 커맨드 파싱 & 설정
    CLI->>Cobra: 플래그 파싱
    Cobra->>Cobra: 설정 우선순위 적용<br/>(플래그 > 환경변수 > 설정파일 > 기본값)

    Note over Cobra, Conn: 2단계: gRPC 연결 수립
    Cobra->>Conn: 서버 주소 결정
    alt port-forward 모드
        Conn->>Conn: kubectl port-forward 실행
    else 직접 연결
        Conn->>Conn: TLS/인증 설정
    end
    Conn->>Server: gRPC 연결 수립

    Note over CLI, Filter: 3단계: 필터 구성
    CLI->>CLI: --verdict drop → FlowFilter 생성
    CLI->>Server: GetFlows(whitelist=[{verdict: DROPPED}], follow=true)

    Note over Server, BPF: 4단계: 서버 측 처리
    loop 이벤트 루프
        BPF->>Observer: perf ring buffer → MonitorEvent
        Observer->>Observer: OnMonitorEvent hooks 실행
        Observer->>Parser: Decode(MonitorEvent)
        Parser->>Parser: L3/L4 파싱 + K8s 메타데이터 enrichment
        Parser-->>Observer: Flow (Protobuf)
        Observer->>Observer: OnDecodedFlow hooks 실행
        Observer->>Filter: whitelist/blacklist 적용
        alt 필터 통과
            Filter-->>Observer: 매치
            Observer->>Observer: OnFlowDelivery hooks 실행
            Observer->>Ring: 저장
            Observer->>Server: Flow 스트리밍
        else 필터 불일치
            Filter-->>Observer: 스킵
        end
    end

    Note over Server, Printer: 5단계: 클라이언트 측 출력
    Server-->>CLI: GetFlowsResponse (stream)
    CLI->>Printer: 포맷 적용 (compact/json/table)
    Printer->>User: 터미널 출력
```

---

## 2. `hubble status` 커맨드 흐름

서버 상태 확인은 단순한 unary RPC입니다.

```mermaid
sequenceDiagram
    actor User as 사용자
    participant CLI as Hubble CLI
    participant Conn as conn 패키지
    participant Health as gRPC Health
    participant Server as Hubble Server

    User->>CLI: hubble status

    CLI->>Conn: gRPC 연결 수립
    Conn->>Health: Health.Check("observer.Observer")
    Health-->>CLI: HealthCheckResponse

    CLI->>Server: ServerStatus(ServerStatusRequest)
    Server-->>CLI: ServerStatusResponse
    Note over CLI: num_flows, max_flows,<br/>seen_flows, uptime,<br/>version, flows_rate

    CLI->>User: 상태 정보 출력
```

---

## 3. 멀티 노드 Relay 흐름

Relay를 통한 클러스터 전체 플로우 관찰 시퀀스입니다.

```mermaid
sequenceDiagram
    actor User as 사용자
    participant CLI as Hubble CLI
    participant Relay as Hubble Relay
    participant PeerMgr as Peer Manager
    participant Node1 as Node 1 Hubble
    participant Node2 as Node 2 Hubble
    participant Node3 as Node 3 Hubble
    participant PQ as Priority Queue

    User->>CLI: hubble observe --server relay:4245 --follow

    CLI->>Relay: GetFlows(follow=true)

    Note over Relay, PeerMgr: Relay가 클러스터 노드 연결 관리
    Relay->>PeerMgr: 연결된 피어 목록 조회
    PeerMgr-->>Relay: [Node1, Node2, Node3]

    par 병렬 스트림 수립
        Relay->>Node1: GetFlows(follow=true)
        Relay->>Node2: GetFlows(follow=true)
        Relay->>Node3: GetFlows(follow=true)
    end

    loop 이벤트 병합 루프
        Node1-->>PQ: Flow (t=100ms)
        Node3-->>PQ: Flow (t=102ms)
        Node2-->>PQ: Flow (t=105ms)

        Note over PQ: 타임스탬프 기준 정렬
        PQ-->>Relay: Flow (t=100ms, Node1)
        Relay-->>CLI: GetFlowsResponse

        PQ-->>Relay: Flow (t=102ms, Node3)
        Relay-->>CLI: GetFlowsResponse
    end

    Note over Relay, Node2: 노드 장애 시
    Node2--xRelay: 연결 끊김
    Relay-->>CLI: NodeStatusEvent(NODE_UNAVAILABLE, "Node2")
    CLI->>User: "Node2: unavailable" 표시

    Note over Relay, Node2: 노드 복구 시
    Node2->>Relay: 재연결
    Relay-->>CLI: NodeStatusEvent(NODE_CONNECTED, "Node2")
```

### 왜 Priority Queue인가?

각 노드의 Flow 이벤트는 네트워크 지연 등으로 순서가 뒤바뀔 수 있습니다.
Priority Queue(min-heap)를 사용하여 타임스탬프 기준으로 정렬함으로써:
- 클라이언트는 항상 시간순으로 정렬된 통합 스트림을 받음
- 약간의 버퍼링 지연이 있지만, 데이터 일관성 보장

---

## 4. Parser 디코딩 흐름

BPF raw 이벤트를 Flow protobuf로 변환하는 과정입니다.

```mermaid
sequenceDiagram
    participant Observer as LocalObserver
    participant Parser as Parser
    participant L34 as L3/L4 Parser<br/>(threefour)
    participant L7 as L7 Parser<br/>(seven)
    participant DBG as Debug Parser
    participant Sock as Socket Parser
    participant DNS as DNSGetter
    participant EP as EndpointGetter
    participant ID as IdentityGetter
    participant SVC as ServiceGetter

    Observer->>Parser: Decode(MonitorEvent)

    Note over Parser: 이벤트 타입 판별
    alt Drop / Trace / PolicyVerdict / Capture
        Parser->>L34: Decode(payload)
        L34->>L34: 이더넷 헤더 파싱
        L34->>L34: IP 헤더 파싱 (v4/v6)
        L34->>L34: TCP/UDP/ICMP/SCTP 파싱

        Note over L34, SVC: K8s 메타데이터 enrichment
        L34->>EP: GetEndpointInfo(srcIP)
        EP-->>L34: Pod 이름, 네임스페이스, 레이블
        L34->>EP: GetEndpointInfo(dstIP)
        EP-->>L34: Pod 이름, 네임스페이스, 레이블
        L34->>ID: GetIdentity(securityID)
        ID-->>L34: Identity 정보
        L34->>DNS: GetNamesOf(epID, ip)
        DNS-->>L34: DNS 이름 목록
        L34->>SVC: GetServiceByAddr(ip, port)
        SVC-->>L34: Service 이름

        L34-->>Parser: Flow (enriched)

    else L7 이벤트
        Parser->>L7: Decode(payload)
        L7->>L7: 프로토콜 감지 (DNS/HTTP)
        alt DNS
            L7->>L7: DNS 쿼리/응답 파싱
        else HTTP
            L7->>L7: HTTP 메서드/경로/상태 파싱
        end
        L7-->>Parser: Flow (with L7 data)

    else Debug 이벤트
        Parser->>DBG: Decode(payload)
        DBG-->>Parser: DebugEvent

    else Socket Trace
        Parser->>Sock: Decode(payload)
        Sock-->>Parser: Flow (socket info)
    end

    Parser-->>Observer: Event (decoded)
```

---

## 5. 설정 로드 시퀀스

Hubble CLI의 설정 우선순위가 적용되는 과정입니다.

```mermaid
sequenceDiagram
    participant User as 사용자
    participant Cobra as Cobra
    participant Viper as Viper
    participant File as 설정 파일
    participant Env as 환경 변수
    participant Flag as CLI 플래그

    User->>Cobra: hubble observe --server relay:4245

    Note over Cobra, Viper: 초기화
    Cobra->>Viper: 설정 소스 등록
    Viper->>Viper: 기본값 설정<br/>(server=localhost:4245, timeout=5s, ...)

    Note over Viper, File: 설정 파일 탐색
    Viper->>File: ./config.yaml 확인
    Viper->>File: $XDG_CONFIG_HOME/hubble/config.yaml 확인
    Viper->>File: ~/.hubble/config.yaml 확인
    File-->>Viper: server: internal-relay:4245

    Note over Viper, Env: 환경 변수 확인
    Viper->>Env: $HUBBLE_SERVER 확인
    Env-->>Viper: (미설정)

    Note over Viper, Flag: CLI 플래그 적용
    Viper->>Flag: --server 플래그 확인
    Flag-->>Viper: relay:4245

    Note over Viper: 최종 우선순위 결정:<br/>Flag(relay:4245) > Env(없음) > File(internal-relay:4245) > Default(localhost:4245)

    Viper-->>Cobra: server = "relay:4245"
```

---

## 6. Port-Forward 연결 시퀀스

`--port-forward` 옵션 사용 시의 연결 과정입니다.

```mermaid
sequenceDiagram
    actor User as 사용자
    participant CLI as Hubble CLI
    participant Conn as conn 패키지
    participant K8s as Kubernetes API
    participant Pod as Hubble Pod
    participant Server as Hubble gRPC

    User->>CLI: hubble observe --port-forward

    CLI->>Conn: 연결 요청 (port-forward 모드)

    Note over Conn, K8s: kubectl port-forward 설정
    Conn->>K8s: Pod 목록 조회<br/>(label: k8s-app=cilium)
    K8s-->>Conn: Cilium Pod 목록

    Conn->>Conn: Pod 선택 (첫 번째 Ready Pod)
    Conn->>K8s: PortForward 요청<br/>(localPort:randomPort → podPort:4245)
    K8s->>Pod: 터널 수립

    Note over Conn, Server: 로컬 gRPC 연결
    Conn->>Server: gRPC 연결 (localhost:randomPort)
    Server-->>Conn: 연결 수립

    CLI->>Server: GetFlows(...)
    Server-->>CLI: Flow 스트림
    CLI->>User: 출력
```

### 왜 Port-Forward인가?

- 클러스터 외부에서 Hubble Server에 직접 접근하려면 Ingress/LoadBalancer 설정이 필요
- Port-Forward는 추가 인프라 없이 `kubectl` 인증만으로 접근 가능
- 개발/디버깅 시 가장 빠르게 사용할 수 있는 방법

---

## 직접 실행해보기 (PoC)

| PoC | 실행 | 학습 내용 |
|-----|------|----------|
| [poc-observer-pipeline](poc-observer-pipeline/) | `cd poc-observer-pipeline && go run main.go` | 5단계 이벤트 파이프라인 (Hook → Decode → Filter → Deliver) |
| [poc-grpc-streaming](poc-grpc-streaming/) | `cd poc-grpc-streaming && go run main.go` | Server Streaming 패턴 (observe 커맨드 흐름) |
| [poc-config-priority](poc-config-priority/) | `cd poc-config-priority && go run main.go` | 설정 로드 우선순위 시퀀스 |
| [poc-graceful-shutdown](poc-graceful-shutdown/) | `cd poc-graceful-shutdown && go run main.go` | 시그널 처리, context 취소 전파, errgroup |
