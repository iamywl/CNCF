# 05. API & 인터페이스 명세 (API Reference)

## gRPC 서비스 정의

Hubble은 gRPC를 통해 3개의 핵심 서비스를 제공합니다.

---

## 1. Observer 서비스

네트워크 플로우 관찰의 핵심 API입니다.

### RPC 메서드

| 메서드 | 타입 | 설명 |
|--------|------|------|
| `GetFlows` | Server Streaming | 네트워크 플로우 스트리밍 |
| `GetAgentEvents` | Server Streaming | Cilium 에이전트 이벤트 스트리밍 |
| `GetDebugEvents` | Server Streaming | 데이터플레인 디버그 이벤트 스트리밍 |
| `GetNodes` | Unary | 클러스터 노드 목록 조회 |
| `GetNamespaces` | Unary | 활성 네임스페이스 목록 조회 |
| `ServerStatus` | Unary | 서버 상태 조회 |

### GetFlows

가장 많이 사용되는 API. Flow 이벤트를 필터링하여 스트리밍합니다.

**Request:**

```protobuf
message GetFlowsRequest {
    uint64 number = 1;           // 반환할 플로우 수 (0 = 제한 없음)
    bool follow = 3;             // true: 실시간 스트림, false: 버퍼 조회
    FlowFilter[] whitelist = 5;  // 포함 필터 (OR 결합)
    FlowFilter[] blacklist = 6;  // 제외 필터 (OR 결합)
    Timestamp since = 7;         // 시작 시간
    Timestamp until = 8;         // 종료 시간
    FieldMask field_mask = 9;    // 응답 필드 제한 (성능 최적화)
    Experimental experimental = 10;
}
```

**Response (stream):**

```protobuf
message GetFlowsResponse {
    oneof response_types {
        Flow flow = 1;                    // 플로우 데이터
        NodeStatusEvent node_status = 2;  // 노드 상태 변경 (Relay)
        LostEvent lost_events = 3;        // 이벤트 유실 알림
    }
    string node_name = 1000;
    Timestamp time = 1001;
}
```

**사용 예시:**

```bash
# 최근 20개 dropped 플로우 조회
hubble observe --verdict DROPPED --last 20

# 실시간 DNS 트래픽 관찰
hubble observe --follow --protocol dns

# 특정 Pod의 HTTP 트래픽
hubble observe -f --source-pod default/frontend --protocol http
```

### GetAgentEvents

Cilium 에이전트의 상태 변경 이벤트를 스트리밍합니다.

```bash
hubble observe agent-events --follow
```

### GetDebugEvents

eBPF 데이터플레인의 디버그 이벤트를 스트리밍합니다.

```bash
hubble observe debug-events --follow
```

### ServerStatus

```protobuf
message ServerStatusResponse {
    uint64 num_flows = 1;           // 현재 버퍼의 플로우 수
    uint64 max_flows = 2;           // 최대 버퍼 용량
    uint64 seen_flows = 3;          // 누적 관찰 플로우 수
    uint64 uptime_ns = 4;           // 서버 가동 시간 (나노초)
    uint64 num_connected_nodes = 5; // 연결된 노드 수 (Relay)
    uint64 num_unavailable_nodes = 6; // 미연결 노드 수
    FlowsRate flows_rate = 7;       // 초당 플로우 처리율
    string version = 8;             // 서버 버전
}
```

---

## 2. Peer 서비스

Hubble 피어(노드) 변경 사항을 스트리밍합니다. 주로 Relay가 내부적으로 사용합니다.

| 메서드 | 타입 | 설명 |
|--------|------|------|
| `Notify` | Server Streaming | 피어 추가/삭제/변경 알림 |

```protobuf
message ChangeNotification {
    string name = 1;
    string address = 2;
    ChangeNotificationType type = 3;  // PEER_ADDED, PEER_DELETED, PEER_UPDATED
    TLS tls = 4;
}
```

---

## 3. Recorder 서비스 (실험적)

패킷 캡처 및 pcap 파일 기록 기능입니다.

```bash
hubble record --fileSink /tmp/capture.pcap
```

---

## CLI 커맨드 상세

### 커맨드 계층 구조

```
hubble
├── observe                    # 네트워크 이벤트 관찰
│   ├── (기본: flows)          # L3/L4 플로우
│   ├── agent-events           # Cilium 에이전트 이벤트
│   └── debug-events           # 데이터플레인 디버그
├── list                       # 객체 목록 조회
│   ├── nodes                  # 클러스터 노드
│   └── namespaces             # 네임스페이스
├── status                     # 서버 상태
├── config                     # 설정 관리
│   ├── view                   # 현재 설정 보기
│   ├── get <key>              # 특정 설정값 조회
│   ├── set <key> <value>      # 설정값 변경
│   └── reset <key>            # 설정 초기화
├── version                    # 버전 정보
├── record                     # 패킷 캡처 (실험적)
└── watch                      # 객체 감시 (개발용, 숨김)
    └── peer                   # 피어 변경 감시
```

### 글로벌 플래그

| 플래그 | 환경변수 | 기본값 | 설명 |
|--------|---------|--------|------|
| `--config` | `HUBBLE_CONFIG` | 자동 탐색 | 설정 파일 경로 |
| `--debug` (`-D`) | `HUBBLE_DEBUG` | false | 디버그 로깅 활성화 |

### 서버 연결 플래그

| 플래그 | 환경변수 | 기본값 | 설명 |
|--------|---------|--------|------|
| `--server` | `HUBBLE_SERVER` | `localhost:4245` | Hubble 서버 주소 |
| `--timeout` | `HUBBLE_TIMEOUT` | `5s` | 연결 타임아웃 |
| `--request-timeout` | `HUBBLE_REQUEST_TIMEOUT` | `12s` | Unary RPC 타임아웃 |
| `--tls` | `HUBBLE_TLS` | false | TLS 활성화 |
| `--tls-allow-insecure` | `HUBBLE_TLS_ALLOW_INSECURE` | false | 인증서 검증 생략 |
| `--tls-ca-cert-files` | `HUBBLE_TLS_CA_CERT_FILES` | - | CA 인증서 경로 |
| `--tls-client-cert-file` | `HUBBLE_TLS_CLIENT_CERT_FILE` | - | 클라이언트 인증서 |
| `--tls-client-key-file` | `HUBBLE_TLS_CLIENT_KEY_FILE` | - | 클라이언트 키 |
| `--tls-server-name` | `HUBBLE_TLS_SERVER_NAME` | - | 서버 이름 (SNI) |
| `--basic-auth-username` | `HUBBLE_BASIC_AUTH_USERNAME` | - | Basic Auth 사용자 |
| `--basic-auth-password` | `HUBBLE_BASIC_AUTH_PASSWORD` | - | Basic Auth 비밀번호 |
| `--port-forward` | `HUBBLE_PORT_FORWARD` | false | kubectl port-forward 사용 |
| `--kubeconfig` | `HUBBLE_KUBECONFIG` | - | kubeconfig 경로 |
| `--kube-context` | `HUBBLE_KUBE_CONTEXT` | - | K8s 컨텍스트 |
| `--kube-namespace` | `HUBBLE_KUBE_NAMESPACE` | - | K8s 네임스페이스 |

---

## Observe 필터 시스템

### 엔드포인트 필터

| 플래그 | 설명 | 예시 |
|--------|------|------|
| `--source-ip` | 출발지 IP (CIDR 지원) | `--source-ip 10.0.0.0/8` |
| `--destination-ip` | 도착지 IP | `--destination-ip 8.8.8.8` |
| `--source-pod` | 출발지 Pod | `--source-pod default/nginx` |
| `--destination-pod` | 도착지 Pod | `--destination-pod kube-system/coredns` |
| `--source-label` | 출발지 K8s 레이블 | `--source-label app=frontend` |
| `--destination-label` | 도착지 K8s 레이블 | `--destination-label k8s:io.kubernetes.pod.namespace=kube-system` |
| `--source-service` | 출발지 서비스 | `--source-service default/web` |
| `--destination-service` | 도착지 서비스 | `--destination-service default/api` |
| `--source-identity` | 출발지 보안 아이덴티티 | `--source-identity 12345` |

### 프로토콜 필터

| 플래그 | 설명 | 예시 |
|--------|------|------|
| `--protocol` | L4 프로토콜 | `--protocol tcp`, `--protocol udp` |
| `--source-port` | 출발지 포트 | `--source-port 8080` |
| `--destination-port` | 도착지 포트 | `--destination-port 53` |
| `--ip-version` | IP 버전 | `--ip-version 4`, `--ip-version 6` |

### L7 필터

| 플래그 | 설명 | 예시 |
|--------|------|------|
| `--http-method` | HTTP 메서드 | `--http-method GET` |
| `--http-path` | HTTP 경로 (regex) | `--http-path "/api/v1/.*"` |
| `--http-status-code` | HTTP 상태 코드 | `--http-status-code 500` |
| `--http-url` | HTTP URL (regex) | `--http-url "example.com/api"` |
| `--dns-query` | DNS 쿼리 이름 (regex) | `--dns-query ".*google.com"` |

### 판정/상태 필터

| 플래그 | 설명 | 예시 |
|--------|------|------|
| `--verdict` | 판정 결과 | `--verdict DROPPED` |
| `--drop-reason-desc` | Drop 사유 | `--drop-reason-desc POLICY_DENIED` |
| `--traffic-direction` | 트래픽 방향 | `--traffic-direction ingress` |
| `--reply` | 응답 패킷 여부 | `--reply=true` |

### 고급 필터

| 플래그 | 설명 | 예시 |
|--------|------|------|
| `--cel-expression` | CEL 표현식 | `--cel-expression "flow.verdict == DROPPED"` |
| `--trace-id` | 트레이스 ID | `--trace-id abc123` |
| `--encrypted` | 암호화 여부 | `--encrypted=true` |

---

## 출력 형식

### 지원 포맷

| 포맷 | 플래그 | 설명 | 용도 |
|------|--------|------|------|
| **compact** | `-o compact` | 한 줄 요약 (기본값) | 실시간 모니터링 |
| **json** | `-o json` | JSON 형식 | 파이프라인, 로그 수집 |
| **jsonpb** | `-o jsonpb` | Protobuf JSON | API 디버깅 |
| **dict** | `-o dict` | KEY:VALUE 쌍 | 상세 분석 |
| **table** | `-o table` | 탭 정렬 테이블 | 정리된 보기 |

### compact 출력 예시

```
Feb 27 09:15:23.456: default/frontend:52918 (ID:12345) -> default/backend:8080 (ID:67890)
  to-endpoint FORWARDED (TCP Flags: SYN)

Feb 27 09:15:23.789: default/frontend:52918 (ID:12345) -> 10.0.0.1:443 (world)
  to-stack DROPPED (Policy denied)
```

### 시간 형식 옵션

| 옵션 | 예시 |
|------|------|
| `--time-format StampMilli` | `Jan 2 15:04:05.000` |
| `--time-format RFC3339` | `2025-02-27T09:15:23Z` |
| `--time-format RFC3339Nano` | `2025-02-27T09:15:23.456789Z` |

---

## 실용적인 사용 패턴

### DNS 문제 디버깅

```bash
# DNS 쿼리와 응답 모니터링
hubble observe --protocol dns -o json

# 특정 도메인의 DNS 조회 추적
hubble observe --dns-query "api.example.com" --follow
```

### 네트워크 정책 검증

```bash
# 차단된 모든 트래픽 확인
hubble observe --verdict DROPPED --follow

# 특정 Pod에서 나가는 트래픽 중 차단된 것
hubble observe --source-pod default/myapp --verdict DROPPED

# 정책 이름 포함하여 출력
hubble observe --verdict DROPPED --print-policy-names
```

### 서비스 간 통신 분석

```bash
# frontend → backend 통신 관찰
hubble observe --source-service default/frontend --destination-service default/backend

# 특정 HTTP 경로의 5xx 에러
hubble observe --http-path "/api/v1/users" --http-status-code 500+
```

### 성능 분석

```bash
# 서버 처리율 확인
hubble status

# JSON 출력으로 파이프라인 연동
hubble observe --follow -o json | jq '.flow.l7.http.code'
```

---

## 직접 실행해보기 (PoC)

| PoC | 실행 | 학습 내용 |
|-----|------|----------|
| [poc-cobra-cli](poc-cobra-cli/) | `cd poc-cobra-cli && go run main.go observe --verdict DROPPED` | Cobra 서브커맨드 구조, 플래그 파싱 |
| [poc-filter-chain](poc-filter-chain/) | `cd poc-filter-chain && go run main.go` | Observe 필터 시스템 (Whitelist/Blacklist) |
| [poc-output-formatter](poc-output-formatter/) | `cd poc-output-formatter && go run main.go` | compact/json/dict/tab 출력 형식 (Strategy 패턴) |
| [poc-cidr-filter](poc-cidr-filter/) | `cd poc-cidr-filter && go run main.go` | CIDR/IP 필터링 (netip.Prefix, Contains) |
| [poc-fqdn-matching](poc-fqdn-matching/) | `cd poc-fqdn-matching && go run main.go` | FQDN 와일드카드→정규식, DNS 필터 |
