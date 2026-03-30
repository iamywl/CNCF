# PoC-01: Cilium Hive Cell 아키텍처 시뮬레이션

## 개요

이 PoC는 Cilium Agent의 내부 컴포넌트 관리 프레임워크인 **Hive**의 Cell 아키텍처를
Go 표준 라이브러리만으로 시뮬레이션한다. 6개의 Cell(K8sClient, APIServer,
EndpointManager, PolicyEngine, BPFLoader, MapManager)을 3계층으로 구분하여 등록하고,
의존성 검증 → 레벨 순 정렬 → 순차 시작 → 시그널 대기 → 역순 종료의 전체
라이프사이클을 재현한다.

---

## 배경 지식

### Cilium의 Hive Cell 아키텍처란?

Cilium Agent는 단일 바이너리 안에 수십 개의 서브시스템을 포함한다.
K8s 클라이언트, 엔드포인트 관리, 정책 엔진, BPF 로더, BPF 맵 관리 등이 모두
하나의 프로세스에서 동작한다. 이러한 복잡성을 관리하기 위해 Cilium은 **Hive**라는
자체 DI(의존성 주입) 프레임워크를 개발했다.

Hive의 핵심 단위는 **Cell**이다. 각 Cell은 독립적인 기능 모듈로서 이름, 의존성 목록,
Start/Stop 라이프사이클 훅을 가진다. Hive 컨테이너가 모든 Cell을 관리하며,
시작 시 의존성 그래프를 검증하고 올바른 순서로 초기화를 수행한다.

### 왜 마이크로서비스형 내부 구조를 채택했는가?

Cilium은 초기에 전통적인 init 함수 체인으로 초기화를 수행했다. 그러나 컴포넌트가
늘어나면서 다음 문제가 발생했다:

- **초기화 순서 관리 불가**: 40개 이상의 서브시스템 간 암묵적 의존 관계가 얽힘
- **테스트 격리 불가**: 특정 컴포넌트만 독립적으로 테스트하기 어려움
- **설정 산재**: 전역 변수와 싱글턴으로 설정이 흩어져 추적이 어려움

Hive Cell 아키텍처는 이를 해결한다:

- **DI 프레임워크**: Cell 간 의존성을 명시적으로 선언하여 자동 주입
- **테스트 용이성**: 특정 Cell만 교체하여 단위 테스트 가능 (mock 주입)
- **모듈 교체**: 동일 인터페이스를 구현하는 다른 Cell로 교체 가능
- **라이프사이클 관리**: 시작/종료 순서를 프레임워크가 보장

### Cilium Agent의 3계층 구조

```
┌─────────────────────────────────────────────────┐
│                   Hive Container                 │
│                                                  │
│  ┌─────────────────────────────────────────────┐ │
│  │ [3] Datapath 레이어                         │ │
│  │     BPFLoader, MapManager                   │ │
│  │     - BPF 프로그램 컴파일/로딩              │ │
│  │     - 커널 BPF 맵 생성/관리                 │ │
│  ├─────────────────────────────────────────────┤ │
│  │ [2] ControlPlane 레이어                     │ │
│  │     EndpointManager, PolicyEngine, IPCache  │ │
│  │     - Pod/엔드포인트 상태 관리              │ │
│  │     - 네트워크 정책 평가 및 적용            │ │
│  ├─────────────────────────────────────────────┤ │
│  │ [1] Infrastructure 레이어                   │ │
│  │     K8sClient, APIServer                    │ │
│  │     - Kubernetes API 연결                   │ │
│  │     - Cilium CLI 통신용 Unix 소켓           │ │
│  └─────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────┘

시작 순서: [1] → [2] → [3]  (아래에서 위로)
종료 순서: [3] → [2] → [1]  (위에서 아래로)
```

Infrastructure가 먼저 준비되어야 ControlPlane이 K8s 이벤트를 받을 수 있고,
ControlPlane이 엔드포인트/정책 정보를 확보해야 Datapath가 BPF 프로그램을
올바르게 로딩할 수 있다.

---

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 코드 | 시뮬레이션 방식 |
|------|------------------|----------------|
| Cell | `pkg/hive/cell/cell.go` | `Cell` 인터페이스 (Name, Level, Dependencies, Start, Stop) |
| Hive | `pkg/hive/hive.go` | `Hive` 구조체 (RegisterCell, validateDependencies, Start, Stop) |
| Lifecycle Hook | `pkg/hive/lifecycle.go` | `HookInterface` 인터페이스 + `HookContext` |
| Config 바인딩 | `pkg/hive/cell/config.go` | `Config` 구조체로 key-value 설정 주입 |
| Cell 레벨 | Agent 내부 계층 구조 | `CellLevel` 상수 (Infrastructure=0, ControlPlane=1, Datapath=2) |
| K8s 클라이언트 | `pkg/k8s/client/cell.go` | `K8sClientCell` — API 서버 연결 시뮬레이션 |
| API 서버 | `pkg/api/cell.go` | `APIServerCell` — Unix 소켓 대기 시뮬레이션 |
| 엔드포인트 관리 | `pkg/endpoint/cell.go` | `EndpointManagerCell` — Pod 엔드포인트 복원 |
| 정책 엔진 | `pkg/policy/cell.go` | `PolicyEngineCell` — 정책 규칙 로드 |
| BPF 로더 | `pkg/datapath/loader/cell.go` | `BPFLoaderCell` — BPF 오브젝트 파일 로딩 목록 |
| BPF 맵 관리 | `pkg/maps/cell.go` | `MapManagerCell` — cilium_ct4_global 등 맵 생성 |

---

## 아키텍처 및 라이프사이클 다이어그램

```
main()
  │
  ├── Config 생성 (k8s-api-server, policy-enforcement, tunnel-protocol 등)
  │
  ├── Hive 생성
  │     │
  │     ├── RegisterCell(K8sClientCell)      ─┐
  │     ├── RegisterCell(APIServerCell)       ─┤ Infrastructure
  │     ├── RegisterCell(EndpointManagerCell) ─┐
  │     ├── RegisterCell(PolicyEngineCell)    ─┤ ControlPlane
  │     ├── RegisterCell(BPFLoaderCell)       ─┐
  │     └── RegisterCell(MapManagerCell)      ─┤ Datapath
  │
  ├── Hive.Start()
  │     │
  │     ├── validateDependencies()   ← 모든 Cell의 deps가 등록되었는지 검증
  │     ├── sortCells()              ← Level 기준 버블 소트 (안정 정렬)
  │     │
  │     ├── [Infrastructure]
  │     │     ├── k8s-client.Start()    → API 서버 연결
  │     │     └── api-server.Start()    → Unix 소켓 리스닝
  │     ├── [ControlPlane]
  │     │     ├── endpoint-manager.Start() → 엔드포인트 복원
  │     │     └── policy-engine.Start()    → 정책 규칙 로드
  │     └── [Datapath]
  │           ├── bpf-loader.Start()    → BPF 프로그램 로딩
  │           └── map-manager.Start()   → BPF 맵 초기화
  │
  ├── 시그널 대기 (SIGINT/SIGTERM 또는 2초 타이머)
  │
  └── Hive.Stop()  ← 역순: map-manager → bpf-loader → policy → endpoint → api → k8s
```

---

## 코드 해설

### 1. `CellLevel` 상수와 Cell 인터페이스 (L79-107)

Cell 인터페이스는 `Name()`, `Level()`, `Dependencies()`, `Start()`, `Stop()` 5개
메서드를 정의한다. `CellLevel`은 iota로 Infrastructure(0) → ControlPlane(1) →
Datapath(2) 순서를 부여한다. Hive가 이 레벨값을 기준으로 시작 순서를 결정하므로,
각 Cell은 자신이 속한 계층을 반드시 선언해야 한다.

### 2. `Hive.validateDependencies()` (L333-346)

모든 등록된 Cell의 이름을 맵에 수집한 후, 각 Cell의 `Dependencies()` 반환값이
맵에 존재하는지 확인한다. 누락된 의존성이 있으면 에러를 반환하여 시작을 차단한다.
실제 Cilium에서도 Hive 시작 시점에 의존성 그래프 검증을 수행하여,
런타임에 nil 참조로 크래시하는 것을 방지한다.

### 3. `Hive.Start()` 메서드 (L363-409)

시작 과정은 4단계로 구성된다: (1) 의존성 검증, (2) 레벨 순 정렬, (3) 시작 순서 출력,
(4) Cell별 순차 Start 호출. `currentLevel` 변수로 레이어 전환 시점을 감지하여
구분선을 출력한다. 실제 Cilium의 Hive도 동일한 패턴으로 레이어 경계를 로깅한다.

### 4. `BPFLoaderCell.Start()` (L248-261)

실제 Cilium이 커널에 로딩하는 BPF 오브젝트 파일 이름(bpf_lxc.o, bpf_host.o,
bpf_overlay.o, bpf_network.o)을 그대로 사용한다. 각 파일의 용도(엔드포인트 TC,
호스트 네트워킹, VXLAN 오버레이, 네트워크 정책)를 주석으로 명시하여,
Cilium의 데이터패스 구성을 파악할 수 있다.

### 5. `MapManagerCell.Start()` (L284-296)

실제 Cilium BPF 맵 이름과 max entries 값을 반영한다. cilium_ct4_global(524288),
cilium_ipcache(512000), cilium_policy(16384) 등은 Cilium의 기본 설정값이다.
Connection Tracking, IP-Identity 매핑, 정책, 엔드포인트, L4 서비스 맵의 역할을
주석으로 설명하여 BPF 맵 체계 전체를 조망할 수 있다.

---

## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-01-architecture
go run main.go
```

주요 출력 발췌:

```
╔══════════════════════════════════════════════════════╗
║  Cilium Hive Cell 아키텍처 시뮬레이션               ║
║  Infrastructure → ControlPlane → Datapath           ║
╚══════════════════════════════════════════════════════╝

=== 설정 ===
  k8s-api-server:    https://10.96.0.1:443
  policy-enforcement: default
  tunnel-protocol:   vxlan

=== Hive 시작 순서 ===
  1. [Infrastructure] k8s-client (의존: (없음))
  2. [Infrastructure] api-server (의존: (없음))
  3. [ControlPlane] endpoint-manager (의존: k8s-client)
  4. [ControlPlane] policy-engine (의존: k8s-client, endpoint-manager)
  5. [Datapath] bpf-loader (의존: endpoint-manager, policy-engine)
  6. [Datapath] map-manager (의존: bpf-loader)

=== Cell 시작 ===

--- Infrastructure 레이어 시작 ---
  [k8s-client] Kubernetes API 서버 연결: https://10.96.0.1:443
  [api-server] Unix 소켓에서 대기: /var/run/cilium/cilium.sock

--- ControlPlane 레이어 시작 ---
  [endpoint-manager] 엔드포인트 관리자 초기화, K8s 워치 시작
  [endpoint-manager] 기존 엔드포인트 2개 복원됨
  [policy-engine] 정책 엔진 시작 (모드: default)
  [policy-engine] 5개 정책 규칙 로드됨

--- Datapath 레이어 시작 ---
  [bpf-loader] BPF 프로그램 컴파일 및 로딩 시작
  [bpf-loader] 로딩 완료: bpf_lxc.o
  ...
  [map-manager] BPF 맵 초기화
  [map-manager] 맵 생성: cilium_ipcache              (max entries: 512000)
  ...

=== Hive 시작 완료 ===

=== Cilium Agent 실행 중 (Ctrl+C 또는 2초 후 자동 종료) ===

=== Cell 정지 (역순) ===
  [map-manager] 6개 BPF 맵 정리
  [bpf-loader] 4개 BPF 프로그램 언로드
  [policy-engine] 정책 엔진 종료
  [endpoint-manager] 2개 엔드포인트 정리 중
  [api-server] API 서버 종료
  [k8s-client] Kubernetes API 서버 연결 해제

=== Hive 정지 완료 ===
```

2초 타이머 만료 후 자동으로 Graceful Shutdown이 수행된다.
Ctrl+C를 누르면 즉시 종료 흐름이 시작된다.

---

## 핵심 포인트

1. **계층적 시작 순서 보장**: Infrastructure가 준비되어야 ControlPlane이 K8s 이벤트를
   수신할 수 있고, ControlPlane이 엔드포인트/정책 정보를 확보해야 Datapath가
   올바른 BPF 프로그램을 생성할 수 있다. Hive는 CellLevel로 이 순서를 강제한다.

2. **역순 종료(Graceful Shutdown)**: 종료 시에는 Datapath(BPF 맵/프로그램)부터
   먼저 정리하여 트래픽 유실을 최소화한 뒤, ControlPlane과 Infrastructure를
   순서대로 종료한다. 이는 실제 Cilium에서도 동일한 패턴이다.

3. **명시적 의존성 선언과 사전 검증**: 각 Cell이 Dependencies()로 의존 대상을
   명시하고, Hive가 시작 전에 그래프를 검증한다. 이를 통해 런타임 nil 참조
   크래시를 원천 차단한다.

4. **DI를 통한 테스트 용이성**: Cell 인터페이스를 구현하는 mock을 주입하면
   특정 레이어만 격리하여 테스트할 수 있다. 예를 들어 K8sClientCell을
   fake 구현으로 교체하면 실제 K8s 클러스터 없이 ControlPlane 테스트가 가능하다.

5. **설정 중앙화**: Config 구조체가 모든 Cell에 주입되므로, 설정값이 전역 변수로
   산재하지 않는다. 실제 Cilium에서는 cilium-config ConfigMap이 이 역할을 한다.

---

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium | 이 PoC |
|------|-------------|--------|
| DI 프레임워크 | uber/dig 기반 Hive (리플렉션 + 타입 기반 주입) | 수동 Cell 등록 및 문자열 의존성 |
| Cell 수 | 40개 이상의 Cell | 6개 Cell로 축소 |
| Config | `cell.Config[T]`로 타입 안전한 설정 바인딩 | `map[string]string` 기반 단순 Config |
| 의존성 해결 | 타입 기반 자동 주입 (생성자 인자로 선언) | 문자열 이름 기반 수동 검증 |
| BPF 로딩 | LLVM 컴파일 + bpf() 시스콜로 커널 로딩 | 문자열 출력으로 시뮬레이션 |
| BPF 맵 | pinned map으로 커널에 실제 생성 | map[string]int로 메타데이터만 표현 |
| Health Check | Cell별 health reporter로 상태 모니터링 | 미구현 |
| Metrics | Prometheus 메트릭 Cell별 자동 등록 | 미구현 |
| 에러 처리 | Cell 시작 실패 시 부분 롤백 지원 | 에러 시 즉시 종료 (롤백 없음) |
| 동시성 | 같은 레벨 내 병렬 시작 가능 | 모든 Cell 순차 시작 |
