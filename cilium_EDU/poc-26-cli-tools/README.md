# PoC-26: Cilium CLI 도구 아키텍처 시뮬레이션

## 개요

이 PoC는 Cilium 프로젝트의 CLI 도구 생태계(cilium-cli, cilium-dbg, bugtool)가 내부적으로 어떻게 구성되어 있는지를
Go 표준 라이브러리만으로 시뮬레이션한다. cobra 기반의 계층적 커맨드 디스패치, 클러스터 상태 진단,
연결성 테스트, Hubble 플로우 관찰, 네트워크 정책 조회, WireGuard 암호화 상태 확인 등
운영 환경에서 가장 빈번하게 사용되는 CLI 명령 7개를 재현한다.

## 배경 지식

### Cilium CLI 도구의 역할

Cilium은 eBPF 기반의 쿠버네티스 네트워킹/보안/관측성 솔루션으로, 운영자가 클러스터를
관리하기 위해 여러 CLI 도구를 제공한다.

| 도구 | 역할 | 주요 사용 시나리오 |
|------|------|-------------------|
| **cilium-cli** | 클러스터 설치/관리/진단 통합 도구 | `cilium install`, `cilium status`, `cilium connectivity test` |
| **cilium-dbg** | 에이전트 디버깅 전용 도구 | BPF 맵 덤프, 엔드포인트 상태 조회, 정책 트레이싱 |
| **cilium-bugtool** | 장애 분석용 정보 수집 도구 | 로그/설정/BPF 맵을 tar.gz로 묶어 지원팀에 전달 |

### 운영 환경에서의 중요성

- **장애 대응 1순위**: `cilium status`로 전체 클러스터 건강 상태를 10초 내에 파악
- **네트워크 검증**: `cilium connectivity test`로 pod-to-pod, pod-to-service, DNS,
  네트워크 정책 등 9종 이상의 연결 시나리오를 자동 검증
- **관측성 확인**: `cilium hubble observe`로 실시간 트래픽 흐름과 drop 사유 추적
- **암호화 감사**: `cilium encrypt status`로 WireGuard/IPsec 노드간 암호화 상태 확인

### cobra 기반 커맨드 디스패치

Cilium CLI는 spf13/cobra 라이브러리를 사용하여 계층적 서브커맨드 트리를 구성한다.
루트 커맨드(`cilium`)에 `status`, `connectivity`, `hubble` 등의 서브커맨드가 등록되고,
각 서브커맨드는 다시 하위 커맨드를 가질 수 있다. 사용자가 입력한 인자를 트리를 따라 탐색하며
최종 매칭된 커맨드의 RunFunc를 실행하는 구조이다.

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 코드 | 시뮬레이션 방식 |
|------|------------------|----------------|
| cobra 커맨드 트리 | `cilium-cli/cmd/` (cobra.Command 등록) | `Command` 구조체로 계층적 서브커맨드 트리 구현 |
| 클러스터 상태 진단 | `cilium-cli/status/` (K8s API로 DaemonSet/Pod 조회) | `ClusterStatus`/`NodeStatus` 모델로 노드별 컴포넌트 상태 시뮬레이션 |
| 연결성 테스트 | `cilium-cli/connectivity/` (테스트 Pod 배포 후 실제 통신) | `ConnectivityTest` 구조체로 9종 테스트 시나리오 시뮬레이션 |
| Hubble 플로우 관찰 | `cilium-cli/hubble/` (gRPC로 Hubble Relay에 연결) | 랜덤 생성된 L3/L4, L7, DNS 플로우 출력 |
| 네트워크 정책 조회 | `cilium-cli/cmd/policy.go` (CRD 조회) | 하드코딩된 CiliumNetworkPolicy/ClusterwidePolicy 목록 출력 |
| 노드간 암호화 상태 | `cilium-cli/encrypt/` (WireGuard 키/상태 조회) | WireGuard 모드의 노드별 공개키/연결 상태 시뮬레이션 |
| 출력 포매팅 | tabwriter 기반 정렬된 테이블 출력 | `text/tabwriter`로 동일한 방식의 테이블 렌더링 |

## 아키텍처 다이어그램

```
┌─────────────────────────────────────────────────────────────┐
│                    main() 진입점                             │
│  buildCLI() → 커맨드 트리 구성 → Execute() 순차 실행          │
└─────────────────┬───────────────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────────────┐
│              Command 트리 (cobra 시뮬레이션)                  │
│                                                             │
│  cilium (root)                                              │
│  ├── status           → cmdStatus()                         │
│  ├── connectivity                                           │
│  │   └── test         → cmdConnectivityTest()               │
│  ├── hubble                                                 │
│  │   ├── status       → cmdHubbleStatus()                   │
│  │   └── observe      → cmdHubbleObserve()                  │
│  ├── policy                                                 │
│  │   └── get          → cmdPolicyGet()                      │
│  └── encrypt                                                │
│      └── status       → cmdEncryptStatus()                  │
└─────────────────┬───────────────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────────────┐
│              Execute(args) 디스패치 흐름                      │
│                                                             │
│  1. args가 비어있으면 → RunFunc 실행 또는 PrintHelp()         │
│  2. args[0]과 매칭되는 서브커맨드 탐색                        │
│  3. 매칭 성공 → sub.Execute(args[1:]) 재귀 호출              │
│  4. 매칭 실패 → 현재 커맨드 RunFunc 실행 또는 에러 반환        │
└─────────────────┬───────────────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────────────┐
│                  데이터 모델 계층                              │
│                                                             │
│  ClusterStatus ─┬── []NodeStatus ─── []ComponentStatus      │
│                 ├── PodCount                                 │
│                 └── PolicyCnt                                │
│                                                             │
│  ConnectivityTest ── Name, From, To, Result, RTT            │
└─────────────────────────────────────────────────────────────┘
```

## 코드 해설

### 1. Command 구조체 - cobra 커맨드 프레임워크 시뮬레이션

```go
type Command struct {
    Use     string
    Short   string
    Long    string
    RunFunc func(cmd *Command, args []string) error
    parent  *Command
    subs    []*Command
    flags   map[string]string
}
```

- **무엇**: cobra.Command의 핵심 필드를 재현한 커맨드 구조체
- **어디**: 실제 Cilium에서는 `spf13/cobra` 패키지의 `cobra.Command`를 사용
- **왜**: CLI 도구의 근간이 되는 계층적 서브커맨드 등록/탐색/실행 패턴을 이해하기 위함.
  `parent` 포인터로 상위 커맨드를 참조하여 `fullName()`에서 전체 경로를 재구성하고,
  `subs` 슬라이스로 하위 커맨드를 관리한다.

### 2. Execute() - 커맨드 디스패치 로직

```go
func (c *Command) Execute(args []string) error
```

- **무엇**: 사용자 입력 인자를 파싱하여 트리에서 매칭되는 커맨드를 찾아 실행
- **어디**: cobra 내부의 `Command.ExecuteC()` → `Find()` → `execute()` 흐름에 대응
- **왜**: `cilium connectivity test`처럼 다단계 서브커맨드를 재귀적으로 탐색하는 것이
  cobra의 핵심 디스패치 패턴이다. args[0]을 소비하며 트리를 내려가고,
  최종 리프 커맨드에 도달하면 RunFunc를 호출한다.

### 3. ClusterStatus / NodeStatus - 상태 모델

```go
type ClusterStatus struct {
    Nodes     []NodeStatus
    PodCount  int
    PolicyCnt int
}
```

- **무엇**: `cilium status` 출력에 필요한 클러스터/노드/컴포넌트 3계층 상태 모델
- **어디**: 실제로는 `cilium-cli/status/` 패키지가 K8s API를 통해 DaemonSet, Pod 상태를 수집
- **왜**: 운영자가 장애 상황에서 가장 먼저 확인하는 것이 노드별 컴포넌트 상태이다.
  cilium-agent, cilium-health, hubble-relay, kube-proxy-replacement 각각의
  OK/Degraded 상태를 한눈에 보여주는 구조를 재현한다.

### 4. runConnectivityTests() - 연결성 테스트 엔진

```go
func runConnectivityTests() []ConnectivityTest
```

- **무엇**: 9종의 네트워크 연결 시나리오를 정의하고 결과(OK/FAIL/BLOCKED)를 생성
- **어디**: 실제 `cilium-cli/connectivity/` 패키지는 테스트용 Pod를 배포한 뒤 실제 통신을 수행
- **왜**: pod-to-pod, pod-to-service, pod-to-external, DNS, 네트워크 정책 등
  쿠버네티스 네트워킹의 모든 주요 경로를 검증하는 것이 Cilium 연결성 테스트의 핵심이다.
  특히 `network-policy-egress` 테스트가 BLOCKED를 반환하는 것은 정책이 정상 작동하는
  증거이므로, 실패가 아닌 기대 결과로 처리한다.

### 5. buildCLI() - 커맨드 트리 조립

```go
func buildCLI() *Command
```

- **무엇**: 루트 커맨드에 status, connectivity, hubble, policy, encrypt 서브커맨드를 등록
- **어디**: 실제 `cilium-cli/cmd/root.go`에서 cobra 커맨드 트리를 구성
- **왜**: CLI 도구의 사용자 경험은 커맨드 트리 설계에 의해 결정된다. Cilium은 기능별로
  최상위 커맨드를 분리하고(status, connectivity, hubble, policy, encrypt),
  각 기능 내에서 동사형 서브커맨드(test, get, observe)를 두는 일관된 패턴을 따른다.

## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-26-cli-tools
go run main.go
```

### 주요 출력 발췌

```
=== Cilium CLI 아키텍처 시뮬레이션 ===

$ cilium
----------------------------------------------------------------------
Usage: cilium [command]

Cilium은 eBPF 기반 네트워킹, 보안, 관측성 솔루션입니다.

Available Commands:
  status          Cilium 클러스터 상태 확인
  connectivity    연결성 테스트 관리
  hubble          Hubble 관측성 도구
  policy          네트워크 정책 관리
  encrypt         노드간 암호화 상태

$ cilium status
----------------------------------------------------------------------
    /\_/\       Cilium:          OK
   /  o o\      Operator:        OK
  (  =^=  )     Hubble Relay:    OK
   )     (      ClusterMesh:     disabled

  Cluster Pods:      127
  Network Policies:  23

  NODE        IP          STATUS
  ----        --          ------
  worker-1    10.0.1.10   OK
  worker-2    10.0.1.11   OK
  worker-3    10.0.1.12   Degraded

$ cilium connectivity test
----------------------------------------------------------------------
  TEST                       FROM             TO                RESULT           RTT
  pod-to-pod                 client/pod-1     echo/pod-1        OK               12ms
  pod-to-service             client/pod-1     echo/svc          OK               18ms
  network-policy-egress      echo/pod-1       external-blocked  BLOCKED (policy) 5ms

  결과: 8 passed, 0 failed, 1 blocked (policy)
```

## 핵심 포인트

1. **계층적 커맨드 디스패치**: cobra의 핵심은 트리 구조에서 args를 하나씩 소비하며
   서브커맨드를 재귀 탐색하는 것이다. `cilium hubble observe`는 root → hubble → observe
   순으로 3단계를 거쳐 최종 핸들러에 도달한다.

2. **3계층 상태 모델**: 클러스터(ClusterStatus) → 노드(NodeStatus) →
   컴포넌트(ComponentStatus) 구조로, 운영자가 장애 범위를 빠르게 좁힐 수 있도록 설계되어 있다.
   전체 OK인지, 어떤 노드가 Degraded인지, 어떤 컴포넌트가 문제인지를 계층적으로 드릴다운한다.

3. **연결성 테스트의 BLOCKED vs FAIL 구분**: 네트워크 정책에 의해 차단된 트래픽은
   BLOCKED (policy)로 표시하며, 이는 정책이 정상 동작하는 증거이다.
   FAIL은 예기치 않은 통신 실패를 의미하며, 이 둘을 구분하는 것이 운영의 핵심이다.

4. **일관된 CLI 설계 패턴**: 모든 서브커맨드가 `tabwriter` 기반 테이블로 출력하고,
   "명사 → 동사" 패턴(policy get, encrypt status, connectivity test)을 따른다.
   이런 일관성이 운영자의 학습 비용을 줄인다.

5. **플래그 시스템**: `Command.flags` 맵으로 `--output json`, `--namespace` 같은
   플래그를 지원하는 구조를 갖추고 있다. 실제 cobra에서는 pflag 라이브러리와 연동하여
   타입 안전한 플래그 파싱을 제공한다.

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium CLI | 이 시뮬레이션 |
|------|----------------|--------------|
| 커맨드 프레임워크 | spf13/cobra + pflag | 자체 Command 구조체 (재귀 탐색만 구현) |
| 상태 수집 | K8s API로 DaemonSet/Pod/Node 실시간 조회 | 하드코딩 + 랜덤 생성 |
| 연결성 테스트 | 실제 테스트 Pod 배포 후 HTTP/TCP/DNS 통신 수행 | 랜덤 결과 생성 (실제 네트워크 통신 없음) |
| Hubble 연동 | gRPC로 Hubble Relay에 연결하여 실시간 플로우 수신 | 랜덤 플로우 데이터 생성 |
| 출력 형식 | --output json/yaml/table 등 다양한 포맷 지원 | 테이블 형식만 지원 |
| 플래그 파싱 | pflag 기반 타입 안전 파싱 (bool, string, int 등) | 단순 map[string]string 저장만 구현 |
| 에러 처리 | 상세한 에러 메시지 + exit code + 제안(suggestion) | 단순 error 반환 |
| 설치/업그레이드 | Helm 차트 기반 클러스터 설치/업그레이드 지원 | 미구현 |
| ClusterMesh | 멀티 클러스터 연결 관리 | 미구현 (상태에서 disabled로만 표시) |
