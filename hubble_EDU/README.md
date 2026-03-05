# Hubble EDU - 네트워크 관측 플랫폼 교육 자료

## 프로젝트 개요

**Hubble**은 Cilium/eBPF 기반의 쿠버네티스 네트워크 관측(observability) 플랫폼이다.
커널 레벨에서 수집된 네트워크 이벤트를 구조화된 Flow 데이터로 변환하여,
클러스터 내 서비스 간 통신을 실시간으로 모니터링하고 분석할 수 있게 해준다.

### 핵심 특징

| 특징 | 설명 |
|------|------|
| eBPF 기반 수집 | 커널 레벨에서 패킷 이벤트를 직접 캡처, 오버헤드 최소화 |
| 구조화된 Flow | L3/L4/L7 프로토콜 정보를 40+ 필드의 protobuf 메시지로 파싱 |
| 실시간 스트리밍 | gRPC 서버 스트리밍으로 Flow를 실시간 관찰 |
| 다중 노드 집계 | Hubble Relay가 클러스터 전체 노드의 Flow를 통합 |
| 풍부한 필터링 | 25+ 필터 타입으로 IP, Pod, FQDN, Label, Protocol 등 세밀한 조건 지정 |
| Prometheus 메트릭 | DNS, HTTP, TCP, Drop, Policy 등 주요 지표를 Prometheus로 노출 |
| Flow Export | Flow 데이터를 파일/외부 시스템으로 내보내기 |
| CLI 도구 | `hubble observe`, `hubble status` 등 직관적 커맨드 라인 인터페이스 |

### 아키텍처 개요

```
+------------------+     +---------------------+     +------------------+
|   eBPF Datapath  |     |   Cilium Agent      |     |   Hubble CLI     |
|                  |     |                     |     |                  |
| TC/XDP Programs  |     |  +---------------+  |     | hubble observe   |
| Perf Ring Buffer-+---->|  | MonitorAgent  |  |     | hubble status    |
|                  |     |  +-------+-------+  |     | hubble list      |
+------------------+     |          |          |     +--------+---------+
                         |          v          |              |
                         |  +-------+-------+  |              | gRPC
                         |  | Observer      |  |              |
                         |  | (LocalServer) |  |     +--------v---------+
                         |  |               |  |     |   Hubble Relay   |
                         |  | Parser ----+  |  |     |                  |
                         |  | L3/L4      |  |  |     | PeerManager      |
                         |  | L7 (HTTP)  |  |<-+---->| PriorityQueue    |
                         |  | Debug      |  |  |gRPC | Aggregation      |
                         |  | Sock       |  |  |     +------------------+
                         |  +-------+----+  |  |
                         |          |       |  |
                         |          v       |  |
                         |  +-------+----+  |  |
                         |  | Ring Buffer |  |  |
                         |  | (Lock-Free) |  |  |
                         |  +-------+----+  |  |
                         |          |       |  |
                         |          v       |  |
                         |  +-------+----+  |  |
                         |  | Filter     |  |  |
                         |  | Whitelist  |  |  |
                         |  | Blacklist  |  |  |
                         |  +-------+----+  |  |
                         |          |       |  |
                         |          v       |  |
                         |  +-------+-------+  |
                         |  | gRPC Server   |  |
                         |  | Observer API  |  |
                         |  | Peer API      |  |
                         |  +---------------+  |
                         +---------------------+
```

### 소스코드 정보

| 항목 | 내용 |
|------|------|
| 저장소 | [github.com/cilium/hubble](https://github.com/cilium/hubble) (CLI), [github.com/cilium/cilium](https://github.com/cilium/cilium) (Server/Core) |
| 언어 | Go |
| 라이선스 | Apache-2.0 |
| CLI 모듈 | `github.com/cilium/cilium/hubble` |
| Core 모듈 | `github.com/cilium/cilium/pkg/hubble` |
| Proto/API | `github.com/cilium/cilium/api/v1` (flow, observer, peer, relay) |
| 빌드 | `make hubble` (Go Build, CGO_ENABLED=0) |

### gRPC 서비스

| 서비스 | 주요 RPC | 설명 |
|--------|----------|------|
| Observer | `GetFlows` | Flow 스트리밍 (whitelist/blacklist 필터, follow, since/until) |
| Observer | `GetAgentEvents` | Cilium 에이전트 이벤트 스트리밍 |
| Observer | `GetDebugEvents` | 데이터패스 디버그 이벤트 스트리밍 |
| Observer | `GetNodes` | 클러스터 노드 목록 및 상태 조회 |
| Observer | `GetNamespaces` | Flow가 관측된 네임스페이스 목록 조회 |
| Observer | `ServerStatus` | 서버 상태 (버전, Flow 수, 가동시간, Flow Rate) |
| Peer | `Notify` | 피어 변경 알림 스트리밍 (ADDED, DELETED, UPDATED) |

---

## 문서 목차

### 기본 문서 (01-06)

| 문서 | 주제 | 설명 |
|------|------|------|
| [01-architecture.md](./01-architecture.md) | 아키텍처 | 전체 아키텍처, CLI-Server-Relay 3계층, Hive Cell 통합 |
| [02-data-model.md](./02-data-model.md) | 데이터 모델 | Flow 메시지 구조, Endpoint, Layer4/7, Verdict, MonitorEvent |
| [03-sequence-diagrams.md](./03-sequence-diagrams.md) | 시퀀스 다이어그램 | 주요 유즈케이스 흐름 (observe, 이벤트 캡처, Relay 집계) |
| [04-code-structure.md](./04-code-structure.md) | 코드 구조 | 디렉토리 구조, 빌드 시스템, 모듈 구조 |
| [05-core-components.md](./05-core-components.md) | 핵심 컴포넌트 | Observer, Parser, Ring Buffer, Filter, Relay, Peer Manager |
| [06-operations.md](./06-operations.md) | 운영 | 설치, CLI 설정, TLS, 모니터링, 트러블슈팅 |

### 심화 문서 (07-18)

| 문서 | 주제 |
|------|------|
| [07-cli-architecture.md](./07-cli-architecture.md) | CLI 아키텍처 (Cobra 커맨드 트리, gRPC 연결, Viper 설정) |
| [08-observer-pipeline.md](./08-observer-pipeline.md) | 옵저버 파이프라인 (이벤트 루프, Hook 체인, 네임스페이스 관리) |
| [09-parser-system.md](./09-parser-system.md) | 파서 시스템 (L3/L4 threefour, L7 seven, Debug, Sock) |
| [10-ring-buffer.md](./10-ring-buffer.md) | 링 버퍼 (Lock-free, atomic, cycle detection, RingReader) |
| [11-filter-chain.md](./11-filter-chain.md) | 필터 체인 (25+ 타입, Whitelist/Blacklist, CEL, BuildFilterList) |
| [12-relay-architecture.md](./12-relay-architecture.md) | 릴레이 아키텍처 (PeerManager, Priority Queue, 다중노드 집계) |
| [13-peer-service.md](./13-peer-service.md) | 피어 서비스 (Notify RPC, ChangeNotification, 알림 버퍼) |
| [14-grpc-api.md](./14-grpc-api.md) | gRPC API (Observer/Peer/Relay 서비스, Flow 메시지, FieldMask) |
| [15-metrics-system.md](./15-metrics-system.md) | 메트릭 시스템 (DNS, HTTP, TCP, Drop, FlowProcessor) |
| [16-exporter-system.md](./16-exporter-system.md) | 익스포터 시스템 (Export 파이프라인, 동적 설정, 파일 로테이션) |
| [17-printer-output.md](./17-printer-output.md) | 프린터/출력 포맷 (compact, dict, jsonpb, table, 색상 코딩) |
| [18-hive-integration.md](./18-hive-integration.md) | Hive Cell 통합 (DI, Job Group, Config, Monitor Consumer) |

### PoC 목록

| PoC | 주제 | 시뮬레이션 대상 |
|-----|------|----------------|
| [poc-01-architecture](./poc-01-architecture/) | 아키텍처 | CLI-Server-Relay 3계층 통신 구조 |
| [poc-02-data-model](./poc-02-data-model/) | 데이터 모델 | Flow/Event/MonitorEvent 구조체 설계 |
| [poc-03-ring-buffer](./poc-03-ring-buffer/) | Ring Buffer | Lock-free 순환 버퍼, atomic 연산, cycle detection |
| [poc-04-parser-pipeline](./poc-04-parser-pipeline/) | Parser 파이프라인 | 이벤트 타입별 파싱 체인 |
| [poc-05-filter-chain](./poc-05-filter-chain/) | 필터 체인 | Whitelist/Blacklist 필터 조합 로직 |
| [poc-06-observer](./poc-06-observer/) | Observer | 이벤트 루프, Hook 체인, 링 버퍼 저장 |
| [poc-07-relay-aggregation](./poc-07-relay-aggregation/) | Relay 집계 | 다중노드 Flow 수집, 정렬, 에러 집계 |
| [poc-08-peer-discovery](./poc-08-peer-discovery/) | Peer Discovery | 노드 발견, 연결 관리, backoff 재시도 |
| [poc-09-grpc-streaming](./poc-09-grpc-streaming/) | gRPC 스트리밍 | 서버 스트리밍 RPC, context 취소 |
| [poc-10-metrics-handler](./poc-10-metrics-handler/) | 메트릭 핸들러 | Prometheus 메트릭 수집/노출 |
| [poc-11-exporter](./poc-11-exporter/) | Exporter | Flow 필터링, 인코딩, 파일 출력 |
| [poc-12-cli-cobra](./poc-12-cli-cobra/) | CLI Cobra | 커맨드 트리, 플래그, Viper 설정 |
| [poc-13-printer-formatter](./poc-13-printer-formatter/) | Printer/Formatter | 다양한 출력 포맷 (JSON, compact, dict) |
| [poc-14-namespace-manager](./poc-14-namespace-manager/) | Namespace Manager | 네임스페이스 추적 및 정렬 |
| [poc-15-lost-event-handler](./poc-15-lost-event-handler/) | Lost Event 처리 | 유실 이벤트 감지, rate-limiting, 보고 |
| [poc-16-priority-queue](./poc-16-priority-queue/) | Priority Queue | 타임스탬프 기반 min-heap 정렬 |

---

## 학습 순서 가이드

1. **README.md** (현재 문서) - 전체 구조 파악
2. **01-architecture.md** - 3계층 아키텍처와 컴포넌트 관계 이해
3. **02-data-model.md** - Flow 메시지와 핵심 데이터 구조 학습
4. **03-sequence-diagrams.md** - 주요 유즈케이스별 흐름 추적
5. **04-code-structure.md** - 소스코드 디렉토리와 빌드 시스템 파악
6. **05-core-components.md** - 핵심 컴포넌트 동작 원리 심화
7. **06-operations.md** - 설치, 설정, 운영 실무
8. **심화 문서 (07-18)** - 관심 서브시스템별 deep-dive
9. **PoC (poc-01~16)** - 핵심 알고리즘 직접 구현 및 실행
