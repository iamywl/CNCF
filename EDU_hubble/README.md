# Hubble 프로젝트 교육 문서 (EDU)

Cilium Hubble 프로젝트의 계층별 문서화입니다.
"왜(Why)" 중심으로 작성되었으며, 코드를 보면 알 수 있는 내용보다는 설계 의도와 아키텍처적 결정 이유에 집중합니다.

---

## 문서 구성

| 문서 | 추상화 수준 | 설명 | PoC 실습 |
|------|-------------|------|----------|
| [01-OVERVIEW.md](01-OVERVIEW.md) | 개요 | 프로젝트 목적, 기술 스택, 핵심 요구사항 | - |
| [02-ARCHITECTURE.md](02-ARCHITECTURE.md) | 설계 (상위) | 시스템 아키텍처 다이어그램, 구성 요소 간 관계 | [gRPC Streaming](poc-grpc-streaming/), [Relay 병합](poc-relay-merge/), [TLS/mTLS](poc-tls-auth/), [Peer 디스커버리](poc-peer-discovery/), [gRPC Interceptor](poc-grpc-interceptor/) |
| [03-DATA-MODEL.md](03-DATA-MODEL.md) | 설계 (상위) | ERD, 핵심 데이터 구조, 프로토콜 계층 | [Flow 구조](poc-flow-structure/), [패킷 파싱](poc-packet-parser/), [Field Mask](poc-fieldmask/) |
| [04-SEQUENCE-DIAGRAMS.md](04-SEQUENCE-DIAGRAMS.md) | 설계 (상위) | 주요 기능별 시퀀스 다이어그램 | [Observer 파이프라인](poc-observer-pipeline/), [Graceful Shutdown](poc-graceful-shutdown/) |
| [05-API-REFERENCE.md](05-API-REFERENCE.md) | 인터페이스 | gRPC API 명세, CLI 커맨드, 필터 시스템 | [Cobra CLI](poc-cobra-cli/), [출력 포맷터](poc-output-formatter/), [CIDR 필터](poc-cidr-filter/), [FQDN 매칭](poc-fqdn-matching/) |
| [06-OPERATIONS.md](06-OPERATIONS.md) | 운영 | 빌드, 배포, 설정, 트러블슈팅 | [설정 우선순위](poc-config-priority/), [Prometheus 메트릭](poc-prometheus-metrics/) |
| [07-CODE-GUIDE.md](07-CODE-GUIDE.md) | 코드 (하위) | 핵심 패키지, 디자인 패턴, 확장 포인트 | [Ring Buffer](poc-ring-buffer/), [Hook](poc-hook-system/), [Getter](poc-getter-interface/), [Filter](poc-filter-chain/) |

---

## 계층별 문서화 전략

```
┌─────────────────────────────────────┐
│       01-OVERVIEW (개요)             │  ← 프로젝트가 "무엇"인지
├─────────────────────────────────────┤
│  02-ARCHITECTURE (아키텍처)          │  ← 구성 요소가 "어떻게 연결"되는지
│  03-DATA-MODEL (데이터 모델)         │  ← 데이터가 "어떤 구조"인지
│  04-SEQUENCE-DIAGRAMS (시퀀스)       │  ← 기능이 "어떤 흐름"으로 실행되는지
├─────────────────────────────────────┤
│  05-API-REFERENCE (API 명세)         │  ← 외부 인터페이스가 "무엇"인지
├─────────────────────────────────────┤
│  06-OPERATIONS (운영)               │  ← "어떻게" 빌드하고 배포하는지
│  07-CODE-GUIDE (코드 가이드)         │  ← 소스코드가 "어떻게" 작동하는지
└─────────────────────────────────────┘
```

---

## 대상 독자

- **새로 합류한 개발자**: 01 → 02 → 03 → 07 순서로 읽기를 권장
- **운영/SRE 엔지니어**: 01 → 06 → 05 순서로 읽기를 권장
- **API 연동 개발자**: 01 → 05 → 04 순서로 읽기를 권장
- **아키텍트**: 01 → 02 → 03 → 04 순서로 읽기를 권장

---

## 다이어그램 렌더링

이 문서의 다이어그램은 [Mermaid.js](https://mermaid.js.org/) 문법으로 작성되었습니다.
GitHub에서는 자동으로 렌더링되며, 로컬에서는 Mermaid 플러그인이 설치된 Markdown 뷰어를 사용하세요.

---

## PoC 실습 프로젝트

각 문서의 핵심 개념을 직접 실행해볼 수 있는 Go 프로그램입니다.
모든 PoC는 **외부 의존성 없이** `go run main.go`로 바로 실행 가능합니다.

| PoC | 관련 문서 | 실행 명령 | 학습 내용 |
|-----|----------|-----------|----------|
| [poc-grpc-streaming](poc-grpc-streaming/) | 02-ARCHITECTURE | `go run main.go` | gRPC Server Streaming (GetFlows 패턴) |
| [poc-relay-merge](poc-relay-merge/) | 02-ARCHITECTURE | `go run main.go` | Priority Queue로 멀티 노드 Flow 병합 |
| [poc-flow-structure](poc-flow-structure/) | 03-DATA-MODEL | `go run main.go` | Flow 계층 구조 (L2/L3/L4/L7, oneof) |
| [poc-observer-pipeline](poc-observer-pipeline/) | 04-SEQUENCE | `go run main.go` | 5단계 이벤트 처리 파이프라인 |
| [poc-cobra-cli](poc-cobra-cli/) | 05-API-REFERENCE | `go run main.go observe --verdict DROPPED` | Cobra 서브커맨드 구조 |
| [poc-config-priority](poc-config-priority/) | 06-OPERATIONS | `MINI_HUBBLE_SERVER=x go run main.go --server=y` | Flag > Env > File > Default 우선순위 |
| [poc-ring-buffer](poc-ring-buffer/) | 07-CODE-GUIDE | `go run main.go` | 순환 버퍼, power-of-2 비트 마스킹 |
| [poc-hook-system](poc-hook-system/) | 07-CODE-GUIDE | `go run main.go` | Hook 체인, stop 반환값으로 중단 |
| [poc-getter-interface](poc-getter-interface/) | 07-CODE-GUIDE | `go run main.go` | 인터페이스 기반 의존성 역전, Mock 테스트 |
| [poc-filter-chain](poc-filter-chain/) | 07-CODE-GUIDE | `go run main.go` | Whitelist/Blacklist, AND/OR 조합 |
| [poc-tls-auth](poc-tls-auth/) | 02-ARCHITECTURE | `go run main.go` | mTLS/TLS 인증, 인증서 체인, TLS 1.3 강제 |
| [poc-prometheus-metrics](poc-prometheus-metrics/) | 06-OPERATIONS | `go run main.go` | Counter/Gauge/Histogram, /metrics 출력 |
| [poc-packet-parser](poc-packet-parser/) | 03-DATA-MODEL | `go run main.go` | 바이너리 패킷 파싱, L2/L3/L4 레이어 |
| [poc-graceful-shutdown](poc-graceful-shutdown/) | 04-SEQUENCE | `go run main.go` | 시그널 처리, context 취소 전파, errgroup |
| [poc-output-formatter](poc-output-formatter/) | 05-API-REFERENCE | `go run main.go` | Strategy 패턴, compact/json/dict/tab 출력 |
| [poc-cidr-filter](poc-cidr-filter/) | 05-API-REFERENCE | `go run main.go` | netip.Prefix CIDR 매칭, IP 필터링 |
| [poc-peer-discovery](poc-peer-discovery/) | 02-ARCHITECTURE | `go run main.go` | Peer 관리, 연결 상태, 지수 백오프 |
| [poc-fieldmask](poc-fieldmask/) | 03-DATA-MODEL | `go run main.go` | Field Mask 트리, 선택적 필드 복사 |
| [poc-grpc-interceptor](poc-grpc-interceptor/) | 02-ARCHITECTURE | `go run main.go` | Unary/Stream Interceptor 체이닝 |
| [poc-fqdn-matching](poc-fqdn-matching/) | 05-API-REFERENCE | `go run main.go` | FQDN 와일드카드→정규식, DNS 필터 |
