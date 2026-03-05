# 06. Cilium 운영

## 개요

Cilium의 설치, 설정, 디버깅, 모니터링, 트러블슈팅을 다룬다.
운영에 필요한 도구, 설정 옵션, 상태 확인 방법을 소스코드 기반으로 설명한다.

## 1. 설치

### Helm 차트 (권장)

**디렉토리**: `install/kubernetes/cilium/`

```bash
# Cilium 설치
helm repo add cilium https://helm.cilium.io/
helm install cilium cilium/cilium --version <version> \
  --namespace kube-system \
  --set operator.replicas=1

# 주요 설정 옵션
helm install cilium cilium/cilium \
  --set kubeProxyReplacement=true \         # kube-proxy 대체
  --set hubble.enabled=true \               # Hubble 옵저버빌리티
  --set hubble.relay.enabled=true \         # Hubble Relay
  --set hubble.ui.enabled=true \            # Hubble UI
  --set encryption.enabled=true \           # WireGuard 암호화
  --set encryption.type=wireguard \
  --set ipam.mode=cluster-pool              # IPAM 모드
```

### cilium CLI

```bash
# cilium CLI로 설치
cilium install

# 상태 확인
cilium status

# 연결성 테스트
cilium connectivity test
```

### 배포 구성

| 컴포넌트 | K8s 리소스 | 인스턴스 수 |
|----------|-----------|------------|
| cilium-agent | DaemonSet | 노드당 1개 |
| cilium-operator | Deployment | 1~2개 (HA) |
| hubble-relay | Deployment | 1개 |
| hubble-ui | Deployment | 1개 (선택) |
| clustermesh-apiserver | Deployment | 1개 (멀티클러스터 시) |

## 2. 설정 관리

### DaemonConfig

**파일**: `pkg/option/config.go:1187`

```go
type DaemonConfig struct {
    BpfDir             string  // BPF 템플릿 파일 디렉토리
    LibDir             string  // Cilium 라이브러리 파일 디렉토리
    RunDir             string  // Cilium 런타임 디렉토리
    StateDir           string  // 엔드포인트 런타임 상태 디렉토리

    EnableXDPPrefilter bool    // XDP 프리필터 활성화
    EnableTCX          bool    // TCX 어태치 활성화 (커널 지원 시)
    EncryptNode        bool    // 노드 IP 트래픽 암호화
    ExternalEnvoyProxy bool    // 외부 Envoy DaemonSet 사용

    DatapathMode string        // 데이터패스 모드
    RoutingMode  string        // 라우팅 모드 (tunnel/native)

    RestoreState bool          // 이전 데몬 상태 복원
    DryMode      bool          // BPF 맵/디바이스 생성 없이 실행

    ClusterName string         // 클러스터 이름
    ClusterID   uint32         // 클러스터 고유 ID

    CTMapEntriesGlobalTCP int  // TCP ConnTrack 맵 최대 엔트리
    ClusterHealthPort     int  // 클러스터 헬스 포트

    // ... 수백 개의 추가 옵션
}
```

### 설정 소스 우선순위

```
1. 커맨드라인 플래그 (--option=value)
2. 환경변수 (CILIUM_OPTION=value)
3. ConfigMap (cilium-config)
4. 기본값 (pkg/defaults/)
```

Viper 라이브러리(`github.com/spf13/viper`)가 이 우선순위를 관리한다.

### 주요 설정 그룹

#### 네트워킹

| 옵션 | 설명 | 기본값 |
|------|------|--------|
| `--tunnel` | 터널 모드 (vxlan, geneve, disabled) | vxlan |
| `--routing-mode` | 라우팅 모드 (tunnel, native) | tunnel |
| `--ipv4-range` | Pod IPv4 CIDR | 10.0.0.0/8 |
| `--ipv6-range` | Pod IPv6 CIDR | - |
| `--enable-ipv6` | IPv6 활성화 | false |

#### 보안

| 옵션 | 설명 | 기본값 |
|------|------|--------|
| `--enable-policy` | 정책 적용 모드 (default, always, never) | default |
| `--policy-audit-mode` | 정책 감사 모드 (위반 시 드롭 대신 로그) | false |
| `--enable-host-firewall` | 호스트 방화벽 | false |

#### 로드밸런싱

| 옵션 | 설명 | 기본값 |
|------|------|--------|
| `--kube-proxy-replacement` | kube-proxy 대체 | false |
| `--bpf-lb-algorithm` | LB 알고리즘 (random, maglev) | random |
| `--bpf-lb-mode` | LB 모드 (snat, dsr, hybrid) | snat |
| `--enable-session-affinity` | 세션 어피니티 | true |

#### IPAM

| 옵션 | 설명 | 기본값 |
|------|------|--------|
| `--ipam` | IPAM 모드 (cluster-pool, eni, azure, multi-pool) | cluster-pool |
| `--cluster-pool-ipv4-cidr` | 클러스터풀 IPv4 CIDR | 10.0.0.0/8 |
| `--cluster-pool-ipv4-mask-size` | 노드당 마스크 크기 | 24 |

## 3. 디버깅 도구

### cilium-dbg (151개 커맨드)

**디렉토리**: `cilium-dbg/cmd/` (151개 파일)

cilium-dbg는 cilium-agent의 UNIX 소켓 API(`/var/run/cilium/cilium.sock`)를
통해 에이전트 상태를 조회하고 관리하는 CLI 도구이다.

```bash
# 주요 커맨드

# 엔드포인트 관리
cilium-dbg endpoint list                 # 엔드포인트 목록
cilium-dbg endpoint get <id>             # 엔드포인트 상세
cilium-dbg endpoint config <id>          # 엔드포인트 설정
cilium-dbg endpoint log <id>             # 엔드포인트 로그

# BPF 맵 조회
cilium-dbg bpf ct list global            # ConnTrack 맵
cilium-dbg bpf nat list                  # NAT 맵
cilium-dbg bpf policy get <ep-id>        # 정책 맵
cilium-dbg bpf endpoint list             # 엔드포인트 맵
cilium-dbg bpf config list               # 설정 맵
cilium-dbg bpf bandwidth list            # 대역폭 맵

# Identity
cilium-dbg identity list                 # Identity 목록
cilium-dbg identity get <id>             # Identity 상세

# 정책
cilium-dbg policy get                    # 현재 정책
cilium-dbg policy trace                  # 정책 트레이스

# 서비스
cilium-dbg service list                  # 서비스 목록
cilium-dbg service get <id>              # 서비스 상세

# 상태
cilium-dbg status                        # 에이전트 상태
cilium-dbg debuginfo                     # 전체 디버그 정보

# BGP
cilium-dbg bgp peers                     # BGP 피어 목록
cilium-dbg bgp routes                    # BGP 라우트

# BPF 인증
cilium-dbg bpf auth list                 # 인증 맵
cilium-dbg bpf auth flush                # 인증 맵 초기화
```

### bugtool

**디렉토리**: `bugtool/`

버그 리포트 수집 도구. 시스템 정보, Cilium 상태, BPF 맵, 로그 등을
압축 파일로 수집한다.

```bash
# 버그 리포트 수집
cilium-bugtool

# 수집 내용:
# - cilium-dbg status/endpoint list/policy get/service list
# - BPF 맵 덤프
# - 시스템 정보 (uname, lsmod, ip addr/route)
# - Cilium 로그
# - K8s 리소스 (CiliumEndpoint, CiliumNetworkPolicy 등)
```

### cilium-health

**디렉토리**: `cilium-health/`

노드 간 연결성을 테스트하는 도구. 각 노드의 cilium-health 엔드포인트와
통신하여 네트워크 연결 상태를 확인한다.

```go
// cilium-health/cmd/root.go
var rootCmd = &cobra.Command{
    Use:   "cilium-health",
    Short: "Cilium Health Client",
    Long:  `Client for querying the Cilium health status API`,
}
```

```bash
# 헬스 상태 조회
cilium-health status

# 출력 예:
# Probe time:   2024-01-01T00:00:00Z
# Nodes:
#   cluster1/node1 (localhost):
#     Host connectivity to 10.0.0.1:
#       ICMP to stack:   OK, RTT=0.5ms
#       HTTP to agent:   OK, RTT=1.2ms
#     Endpoint connectivity to 10.0.0.2:
#       ICMP to stack:   OK, RTT=0.8ms
#       HTTP to agent:   OK, RTT=1.5ms
```

## 4. 모니터링

### Prometheus 메트릭

**파일**: `pkg/metrics/metrics.go`

Cilium은 다양한 서브시스템의 메트릭을 Prometheus 형식으로 노출한다:

```go
// pkg/metrics/metrics.go:27-60
const (
    SubsystemBPF       = "bpf"        // BPF syscall 메트릭
    SubsystemDatapath  = "datapath"   // 데이터플레인 메트릭
    SubsystemAgent     = "agent"      // 에이전트 메트릭
    SubsystemIPCache   = "ipcache"    // IPCache 메트릭
    SubsystemK8s       = "k8s"        // K8s 메트릭
    SubsystemK8sClient = "k8s_client" // K8s 클라이언트 메트릭
    SubsystemKVStore   = "kvstore"    // KVStore 메트릭
)
```

#### 핵심 메트릭 분류

| 카테고리 | 메트릭 접두사 | 설명 |
|----------|-------------|------|
| 데이터플레인 | `cilium_datapath_*` | 드롭/포워드 카운터, 에러 |
| 엔드포인트 | `cilium_endpoint_*` | 엔드포인트 수, 재생성 횟수/시간 |
| 정책 | `cilium_policy_*` | 정책 수, 리비전, 임포트 에러 |
| IPAM | `cilium_ipam_*` | IP 할당/해제 |
| BPF | `cilium_bpf_*` | 맵 연산, syscall 지연 |
| ConnTrack | `cilium_ct_*` | CT 엔트리 수, GC 통계 |
| 서비스 | `cilium_services_*` | 서비스 수 |
| K8s | `cilium_k8s_*` | API 호출, 워처 이벤트 |
| Hubble | `hubble_*` | 플로우 이벤트, 드롭 사유 |
| Operator | `cilium_operator_*` | Identity GC, IPAM 동기화 |

#### BPF 메트릭 맵

```go
// pkg/maps/metricsmap/
// cilium_metrics BPF 맵에서 수집
// 데이터플레인 드롭/포워드 카운터를 Prometheus로 변환
```

`metricsmap.Cell`(`daemon/cmd/cells.go:139`)이 BPF 메트릭 맵(`cilium_metrics`)의
데이터를 주기적으로 읽어 Prometheus 메트릭으로 노출한다.

### Hubble 옵저버빌리티

Hubble은 Cilium의 네트워크 옵저버빌리티 계층이다.

```
+------------+    gRPC    +---------------+    gRPC    +------------+
| hubble CLI |----------->| hubble-relay  |----------->| cilium-    |
| (사용자)   |            | (Deployment)  |            | agent      |
+------------+            | 포트 4245     |            | (각 노드)  |
                          +---------------+            +------------+
                                  |
                                  v
                          +---------------+
                          |  hubble-ui    |
                          |  (웹 UI)      |
                          +---------------+
```

```bash
# Hubble CLI 사용
hubble observe                           # 실시간 플로우 관찰
hubble observe --namespace kube-system   # 특정 네임스페이스
hubble observe --verdict DROPPED         # 드롭된 패킷만
hubble observe --type l7                 # L7 이벤트만
hubble observe --protocol tcp            # TCP만
hubble observe --to-pod deathstar        # 특정 Pod로의 트래픽

# 상태
hubble status                            # Hubble 상태
hubble list nodes                        # 연결된 노드 목록
```

## 5. 상태 확인

### cilium status

```bash
# 에이전트 상태 확인
cilium-dbg status
```

출력 항목:
- **KVStore**: etcd 연결 상태
- **Kubernetes**: API 서버 연결 상태
- **Kubernetes APIs**: 사용 중인 API 그룹
- **KubeProxyReplacement**: kube-proxy 대체 상태
- **Host firewall**: 호스트 방화벽 상태
- **CNI Chaining**: CNI 체이닝 모드
- **CNI Config file**: CNI 설정 파일 상태
- **Cilium health daemon**: 헬스 데몬 상태
- **IPAM**: IP 할당 상태
- **BandwidthManager**: 대역폭 관리 상태
- **Encryption**: 암호화 상태
- **Controller Status**: 각 컨트롤러 상태
- **Proxy Status**: L7 프록시 상태

### 핵심 API 엔드포인트

Agent API(`/var/run/cilium/cilium.sock`):

| 엔드포인트 | 메서드 | 용도 |
|-----------|--------|------|
| `/healthz` | GET | 헬스 체크 (kubelet liveness probe) |
| `/config` | GET | 현재 설정 |
| `/endpoint` | GET | 엔드포인트 목록 |
| `/endpoint/{id}` | GET/PUT/DELETE | 엔드포인트 CRUD |
| `/ipam` | POST | IP 할당 |
| `/ipam/{ip}` | DELETE | IP 해제 |
| `/identity` | GET | Identity 목록 |
| `/policy` | GET | 정책 목록 |
| `/service` | GET | 서비스 목록 |

## 6. 트러블슈팅

### 일반적인 문제와 해결

#### Pod이 Ready 상태가 아닌 경우

```bash
# 1. 엔드포인트 상태 확인
cilium-dbg endpoint list
# State가 "waiting-for-identity" 또는 "regenerating"이면 아직 처리 중

# 2. 에이전트 로그 확인
kubectl -n kube-system logs <cilium-pod> -c cilium-agent

# 3. BPF 프로그램 로드 상태
cilium-dbg bpf endpoint list
```

#### 네트워크 정책이 적용되지 않는 경우

```bash
# 1. 정책 확인
cilium-dbg policy get

# 2. 정책 트레이스
cilium-dbg policy trace \
  --src-identity <src-id> \
  --dst-identity <dst-id> \
  --dport <port>

# 3. BPF policymap 확인
cilium-dbg bpf policy get <endpoint-id>

# 4. 엔드포인트 정책 리비전 확인
cilium-dbg endpoint get <id> | grep policyRevision
```

#### 서비스 로드밸런싱 문제

```bash
# 1. 서비스 목록 확인
cilium-dbg service list

# 2. BPF LB 맵 확인
cilium-dbg bpf lb list

# 3. ConnTrack 확인
cilium-dbg bpf ct list global | grep <vip>
```

#### 성능 이슈

```bash
# 1. BPF 맵 크기 확인
cilium-dbg bpf ct list global | wc -l    # CT 엔트리 수

# 2. 메트릭 확인
# cilium_datapath_drop_total - 드롭 사유별 카운터
# cilium_bpf_map_ops_total - BPF 맵 연산 횟수/지연

# 3. pprof 프로파일링 (기본 비활성화)
# --pprof=true --pprof-address=localhost --pprof-port=6060
```

### 로그 레벨

```bash
# 런타임 로그 레벨 변경
cilium-dbg config DebugEnabled=true

# 또는 Helm 값
--set debug.enabled=true
```

## 7. 업그레이드

### 롤링 업그레이드

```bash
# Helm 업그레이드
helm upgrade cilium cilium/cilium --version <new-version> \
  --namespace kube-system \
  --reuse-values

# 업그레이드 확인
cilium status
cilium connectivity test
```

### 주의사항
- Agent는 DaemonSet이므로 한 번에 하나의 노드씩 롤링 업데이트
- `RestoreState=true` (기본값)이면 에이전트 재시작 시 기존 엔드포인트 상태 복원
- BPF 프로그램은 에이전트 재시작 후 자동 재컴파일/재로드
- 주요 버전 업그레이드 시 CRD 마이그레이션 확인 필요

## 핵심 파일 참조

| 영역 | 파일 | 설명 |
|------|------|------|
| 설정 | `pkg/option/config.go:1187` | DaemonConfig 구조체 |
| 설정 상수 | `pkg/option/config.go:48~` | 설정 옵션 이름 상수 |
| 기본값 | `pkg/defaults/` | 기본 설정 값 |
| Helm 차트 | `install/kubernetes/cilium/` | 설치 매니페스트 |
| cilium-dbg | `cilium-dbg/cmd/` | 디버그 CLI (151개 커맨드) |
| bugtool | `bugtool/` | 버그 리포트 수집 |
| cilium-health | `cilium-health/` | 연결성 테스트 |
| 메트릭 | `pkg/metrics/metrics.go` | Prometheus 메트릭 정의 |
| 메트릭 맵 | `pkg/maps/metricsmap/` | BPF 메트릭 → Prometheus |
| API 서버 | `api/v1/server/` | REST API 서버 |
| Hubble | `pkg/hubble/` | Hubble 서버 셀 |
