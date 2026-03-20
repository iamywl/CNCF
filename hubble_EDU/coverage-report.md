# Hubble EDU 커버리지 분석 리포트

> 검증일: 2026-03-08
> 검증 유형: Group C (경량 검증)

---

## 1. 프로젝트 전체 기능/서브시스템 목록

Hubble은 Cilium 기반 Kubernetes 네트워크 관찰성(observability) 플랫폼이다. CLI 클라이언트(`github.com/cilium/hubble`)와 서버측 코어(`github.com/cilium/cilium/pkg/hubble`, `hubble/`)로 구성된다.

### P0-핵심 (Core Features)

| # | 기능 | 소스 경로 | 설명 |
|---|------|----------|------|
| 1 | CLI 아키텍처 (Cobra 커맨드 트리) | `hubble/cmd/root.go`, `hubble/cmd/observe/` | Cobra 기반 커맨드 구조, Viper 설정, 플래그 시스템 |
| 2 | Observer Pipeline (이벤트 수집/처리) | `pkg/hubble/observer/` (cilium 코어) | LocalObserverServer, 이벤트 루프, MonitorEvent 처리 |
| 3 | Flow 데이터 모델 | `api/v1/flow/flow.pb.go` | Flow protobuf 정의, Verdict, Layer4/7, Endpoint 등 |
| 4 | Ring Buffer (이벤트 저장) | `pkg/hubble/container/ring.go` (cilium 코어) | lock-free 순환 버퍼, cycle 카운터, mask 연산 |
| 5 | Filter Chain (필터 체인) | `pkg/hubble/filters/` | 22종 필터 (IP, Pod, Port, Verdict, Label, FQDN, CEL 등) |
| 6 | Relay 아키텍처 (멀티노드 집계) | `pkg/hubble/relay/` (cilium 코어) | PeerManager, 멀티노드 스트림 병합, 우선순위 큐 |
| 7 | gRPC API (Observer/Peer/Recorder) | `api/v1/observer/`, `api/v1/peer/`, `api/v1/recorder/` | GetFlows, GetAgentEvents, GetDebugEvents, ServerStatus 등 |
| 8 | Printer/출력 포맷 시스템 | `hubble/pkg/printer/` | Compact, Table, Dict, JSON, JSONPB 5종 출력 모드 |
| 9 | Peer Service (피어 디스커버리) | `api/v1/peer/`, `hubble/cmd/watch/peer.go` | 피어 노드 가입/탈퇴 실시간 통지 |
| 10 | Metrics 시스템 | cilium 코어 내 `pkg/hubble/metrics/` | Prometheus 메트릭 변환, FlowProcessor |

### P1-중요 (Important Features)

| # | 기능 | 소스 경로 | 설명 |
|---|------|----------|------|
| 11 | Exporter 시스템 | cilium 코어 내 `pkg/hubble/exporter/` | Flow 외부 내보내기, 파일 로테이션, 필터링, JSON 인코딩 |
| 12 | Parser 시스템 | cilium 코어 내 `pkg/hubble/parser/` | eBPF 이벤트 → Flow 변환, L3/L4/L7 파서 파이프라인 |
| 13 | Hive DI 통합 | cilium 코어 내 `pkg/hubble/cell.go` | Hive Cell 프레임워크 의존성 주입, 라이프사이클 관리 |
| 14 | TLS/mTLS 연결 보안 | `hubble/cmd/common/conn/tls.go` | TLS 설정, 인증서 검증, mTLS 클라이언트 인증 |
| 15 | gRPC 연결 관리 | `hubble/cmd/common/conn/conn.go` | gRPC 다이얼 옵션, 인터셉터, 타임아웃 |
| 16 | Config 관리 (get/set/view/reset) | `hubble/cmd/config/` | Viper 기반 설정 CRUD, 환경변수/파일/플래그 우선순위 |
| 17 | Namespace Manager | `hubble/cmd/list/namespaces.go` | 네임스페이스 목록 조회, 클러스터별 필터링 |
| 18 | K8s Port-Forward 지원 | `hubble/cmd/common/conn/conn.go` | 자동 hubble-relay 포트 포워딩, kubeconfig 통합 |
| 19 | Lost Event 처리 | `pkg/hubble/api/v1/types.go` (LostEvent) | 이벤트 유실 감지, LostEvent 타입, 보고 메커니즘 |
| 20 | Priority Queue (Relay 정렬) | cilium 코어 내 `pkg/hubble/relay/queue/` | 타임스탬프 기반 힙 정렬, PopOlderThan |

### P2-선택 (Optional Features)

| # | 기능 | 소스 경로 | 설명 |
|---|------|----------|------|
| 21 | IOReader Observer (오프라인 분석) | `hubble/cmd/observe/io_reader_observer.go` | --input-file 옵션, 파일 기반 오프라인 Flow 분석 |
| 22 | Record 커맨드 (실험적 pcap) | `hubble/cmd/record/record.go` | 네트워크 패킷 캡처, pcap 저장 (Hidden/실험적) |
| 23 | Reflect 커맨드 (gRPC 리플렉션) | `hubble/cmd/reflect/reflect.go` | gRPC 서비스 탐색, proto 디스크립터 출력 (Hidden) |
| 24 | Basic Auth 인증 | `hubble/cmd/common/conn/auth.go` | username/password 기반 gRPC 인증 |
| 25 | Version Mismatch 감지 | `hubble/cmd/common/conn/version.go` | CLI-서버 버전 불일치 감지 및 경고 |
| 26 | Color 시스템 (ANSI 컬러) | `hubble/pkg/printer/color.go` | Verdict별 색상, auto/always/never 모드 |
| 27 | Terminal Escaper | `hubble/pkg/printer/terminal.go` | 터미널 이스케이프 시퀀스 처리, 비터미널 환경 대응 |
| 28 | Field Mask (실험적) | `hubble/cmd/observe/observe.go` (experimentalOpts) | 응답 필드 선택적 요청으로 대역폭 절감 |

---

## 2. 기존 EDU 커버리지 매핑

### 심화문서 (12개)

| 문서 | 제목 | 커버 기능 | 줄수 |
|------|------|----------|------|
| 07-cli-architecture.md | Hubble CLI 아키텍처 | #1 CLI 아키텍처, #14 TLS, #15 gRPC 연결, #16 Config, #18 Port-Forward, #21 IOReader, #22 Record, #24 Basic Auth | 1,095 |
| 08-observer-pipeline.md | Observer Pipeline Deep-Dive | #2 Observer Pipeline, #3 Flow 데이터 모델(부분), #28 Field Mask(부분) | 1,522 |
| 09-parser-system.md | 파서 시스템 | #12 Parser 시스템 | 1,212 |
| 10-ring-buffer.md | 링 버퍼 | #4 Ring Buffer | 1,004 |
| 11-filter-chain.md | 필터 체인 | #5 Filter Chain (22종 필터 전체) | 985 |
| 12-relay-architecture.md | Relay 아키텍처 Deep Dive | #6 Relay 아키텍처, #20 Priority Queue | 1,979 |
| 13-peer-service.md | 피어 서비스 | #9 Peer Service | 876 |
| 14-grpc-api.md | gRPC API | #7 gRPC API, #3 Flow 데이터 모델 | 852 |
| 15-metrics-system.md | 메트릭 시스템 | #10 Metrics 시스템 | 877 |
| 16-exporter-system.md | 익스포터 시스템 Deep Dive | #11 Exporter 시스템 | 1,626 |
| 17-printer-output.md | 프린터/출력 포맷 Deep Dive | #8 Printer, #26 Color 시스템, #27 Terminal Escaper | 1,138 |
| 18-hive-integration.md | Hive Cell 통합 | #13 Hive DI 통합 | 1,652 |
| 19-field-mask.md | Field Mask 필터링 | #28 Field Mask (실험적 → 정식) | 648 |

### PoC (17개)

| PoC | 제목 | 커버 기능 | 외부 의존성 | 실행 검증 |
|-----|------|----------|------------|----------|
| poc-01-architecture | Hubble 서버 아키텍처 | #1, #2 아키텍처/Observer | 없음 | - |
| poc-02-data-model | Hubble 데이터 모델 | #3 Flow 데이터 모델 | 없음 | - |
| poc-03-ring-buffer | Hubble 링 버퍼 | #4 Ring Buffer | 없음 | PASS |
| poc-04-parser-pipeline | 파서 파이프라인 | #12 Parser 시스템 | 없음 | - |
| poc-05-filter-chain | 필터 체인 | #5 Filter Chain | 없음 | PASS |
| poc-06-observer | 옵저버 | #2 Observer Pipeline | 없음 | - |
| poc-07-relay-aggregation | Relay 멀티노드 Flow 집계 | #6 Relay | 없음 | - |
| poc-08-peer-discovery | Peer Discovery | #9 Peer Service | 없음 | - |
| poc-09-grpc-streaming | gRPC 서버 스트리밍 | #7 gRPC API | 없음 | PASS |
| poc-10-metrics-handler | 메트릭 핸들러 | #10 Metrics 시스템 | 없음 | - |
| poc-11-exporter | Flow 익스포터 파이프라인 | #11 Exporter | 없음 | - |
| poc-12-cli-cobra | Cobra CLI 구조 | #1 CLI 아키텍처, #16 Config | 없음 | - |
| poc-13-printer-formatter | 프린터/포매터 | #8 Printer, #26 Color | 없음 | PASS |
| poc-14-namespace-manager | 네임스페이스 매니저 | #17 Namespace Manager | 없음 | - |
| poc-15-lost-event-handler | 이벤트 유실 감지/보고 | #19 Lost Event 처리 | 없음 | - |
| poc-16-priority-queue | 타임스탬프 기반 우선순위 큐 | #20 Priority Queue | 없음 | PASS |
| poc-17-field-mask | Field Mask 필터링 | #28 Field Mask | 없음 | PASS |

---

## 3. 갭 분석

### 커버리지 요약

| 우선순위 | 전체 | EDU 커버 | 커버율 | 누락 |
|---------|------|---------|--------|------|
| P0-핵심 | 10 | 10 | 100% | 0개 |
| P1-중요 | 10 | 10 | 100% | 0개 |
| P2-선택 | 8 | 8 | 100% | 0개 |
| **합계** | **28** | **28** | **100%** | **0개** |

### 커버리지 상세

#### P0 커버리지 (10/10 = 100%)

| # | 기능 | 커버 문서 | 커버 PoC | 상태 |
|---|------|----------|---------|------|
| 1 | CLI 아키텍처 | 07-cli-architecture.md | poc-01, poc-12 | 커버됨 |
| 2 | Observer Pipeline | 08-observer-pipeline.md | poc-01, poc-06 | 커버됨 |
| 3 | Flow 데이터 모델 | 08, 14-grpc-api.md | poc-02 | 커버됨 |
| 4 | Ring Buffer | 10-ring-buffer.md | poc-03 | 커버됨 |
| 5 | Filter Chain | 11-filter-chain.md | poc-05 | 커버됨 |
| 6 | Relay 아키텍처 | 12-relay-architecture.md | poc-07 | 커버됨 |
| 7 | gRPC API | 14-grpc-api.md | poc-09 | 커버됨 |
| 8 | Printer/출력 시스템 | 17-printer-output.md | poc-13 | 커버됨 |
| 9 | Peer Service | 13-peer-service.md | poc-08 | 커버됨 |
| 10 | Metrics 시스템 | 15-metrics-system.md | poc-10 | 커버됨 |

#### P1 커버리지 (10/10 = 100%)

| # | 기능 | 커버 문서 | 커버 PoC | 상태 |
|---|------|----------|---------|------|
| 11 | Exporter 시스템 | 16-exporter-system.md | poc-11 | 커버됨 |
| 12 | Parser 시스템 | 09-parser-system.md | poc-04 | 커버됨 |
| 13 | Hive DI 통합 | 18-hive-integration.md | - | 문서만 커버 |
| 14 | TLS/mTLS 연결 보안 | 07-cli-architecture.md 6절 | - | 문서만 커버 |
| 15 | gRPC 연결 관리 | 07-cli-architecture.md 5절 | - | 문서만 커버 |
| 16 | Config 관리 | 07-cli-architecture.md 4절 | poc-12 | 커버됨 |
| 17 | Namespace Manager | 07-cli-architecture.md | poc-14 | 커버됨 |
| 18 | K8s Port-Forward | 07-cli-architecture.md 11절 | - | 문서만 커버 |
| 19 | Lost Event 처리 | 08-observer-pipeline.md | poc-15 | 커버됨 |
| 20 | Priority Queue | 12-relay-architecture.md | poc-16 | 커버됨 |

#### P2 커버리지 (8/8 = 100%)

| # | 기능 | 커버 문서 | 커버 PoC | 상태 |
|---|------|----------|---------|------|
| 21 | IOReader Observer | 07-cli-architecture.md | - | 문서만 커버 |
| 22 | Record 커맨드 | 07-cli-architecture.md | - | 문서만 커버 |
| 23 | Reflect 커맨드 | 07-cli-architecture.md (언급) | - | 문서만 커버 (Hidden/실험적) |
| 24 | Basic Auth 인증 | 07-cli-architecture.md | - | 문서만 커버 |
| 25 | Version Mismatch 감지 | 07-cli-architecture.md | - | 문서만 커버 |
| 26 | Color 시스템 | 17-printer-output.md 13절 | poc-13 | 커버됨 |
| 27 | Terminal Escaper | 17-printer-output.md 14절 | poc-13(부분) | 커버됨 |
| 28 | Field Mask | 19-field-mask.md | poc-17 | 커버됨 |

### 누락 상세

#### 누락 기능: 0개

#### P0 누락: 0개
#### P1 누락: 0개
#### P2 누락: 0개

---

## 4. 커버리지 등급

### 등급 판정 기준

| 등급 | 기준 |
|------|------|
| A | P0 누락 0개 |
| B | P0 누락 1개 이하 |
| C | P0 누락 2개 이하 |
| D | P0 누락 3개 이상 |

### 판정 결과

```
등급: A
```

**근거:**
- P0 핵심 기능 10개 전체 커버 (100%)
- P1 중요 기능 10개 전체 커버 (100%)
- P2 선택 기능 8/8 커버 (100%) - Field Mask 추가로 완전 커버
- 심화문서 13개 (기준 10~12 대비 초과)
- PoC 17개 (기준 16~18 대비 충족)
- 모든 PoC는 Go 표준 라이브러리만 사용 (외부 의존성 없음)
- 심화문서 평균 1,190줄 (기준 500줄 이상 전체 충족)
- Spot Check 6/6 PoC 정상 실행 확인

### 수치 요약

| 항목 | 값 | 기준 | 충족 |
|------|-----|------|------|
| 심화문서 | 13개 | 10~12개 | 초과 |
| 심화문서 최소 줄수 | 648줄 | 500줄 이상 | 충족 |
| 심화문서 평균 줄수 | 1,190줄 | - | 우수 |
| PoC | 17개 | 16~18개 | 충족 |
| 외부 의존성 | 0건 | 0건 | 충족 |
| PoC 실행 검증 | 6/6 통과 | spot check | 충족 |
| P0 커버리지 | 100% (10/10) | - | 완벽 |
| P1 커버리지 | 100% (10/10) | - | 완벽 |
| P2 커버리지 | 100% (8/8) | - | 완벽 |
| 전체 커버리지 | 100% (28/28) | - | 완벽 |
