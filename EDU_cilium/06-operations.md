# 6. Cilium 운영 가이드

---

## 설정 체계

Cilium daemon은 ~400개 이상의 설정 옵션을 지원한다.
설정 우선순위 (높은 것이 덮어씀):

```
1. 커맨드라인 플래그        (최우선)
2. 환경 변수 (CILIUM_*)
3. 설정 파일 (ciliumd.yaml)
4. ConfigDir 디렉토리
5. 기본값                   (최하위)
```

소스 위치: `pkg/option/config.go` — `DaemonConfig` 구조체

---

## 주요 설정 옵션

### 네트워킹

| 옵션 | 기본값 | 설명 |
|------|--------|------|
| `--routing-mode` | tunnel | 라우팅 모드 (tunnel / native) |
| `--tunnel-protocol` | vxlan | 터널 프로토콜 (vxlan / geneve) |
| `--ipam` | cluster-pool | IP 할당 방식 (cluster-pool / kubernetes / eni / azure) |
| `--enable-ipv4` | true | IPv4 활성화 |
| `--enable-ipv6` | true | IPv6 활성화 |
| `--cluster-name` | default | 클러스터 이름 (멀티클러스터 시 필수) |
| `--cluster-id` | 0 | 클러스터 ID (멀티클러스터 시 고유값) |

### 보안/정책

| 옵션 | 기본값 | 설명 |
|------|--------|------|
| `--enable-policy` | default | 정책 적용 모드 (default / always / never) |
| `--enable-l7-proxy` | true | L7 프록시 (Envoy) 활성화 |
| `--enable-ipsec` | false | IPsec 암호화 활성화 |
| `--wireguard-enabled` | false | WireGuard 암호화 활성화 |

### 로드밸런싱

| 옵션 | 기본값 | 설명 |
|------|--------|------|
| `--kube-proxy-replacement` | false | kube-proxy 완전 대체 |
| `--bpf-lb-algorithm` | random | LB 알고리즘 (random / maglev) |
| `--bpf-lb-mode` | snat | LB 모드 (snat / dsr / hybrid) |
| `--nodeport-range` | 30000-32767 | NodePort 범위 |

### 관측 (Hubble)

| 옵션 | 기본값 | 설명 |
|------|--------|------|
| `--enable-hubble` | false | Hubble 관측 활성화 |
| `--hubble-socket-path` | /var/run/cilium/hubble.sock | Hubble UNIX 소켓 경로 |
| `--hubble-metrics` | (없음) | 활성화할 Hubble 메트릭 목록 |
| `--hubble-flow-buffer-size` | 4096 | Flow 링 버퍼 크기 |

### BPF

| 옵션 | 기본값 | 설명 |
|------|--------|------|
| `--bpf-map-dynamic-size-ratio` | 0.0 | BPF 맵 동적 크기 비율 |
| `--bpf-ct-global-tcp-max` | 524288 | TCP CT 맵 최대 엔트리 수 |
| `--bpf-ct-global-any-max` | 262144 | 비-TCP CT 맵 최대 엔트리 수 |
| `--bpf-nat-global-max` | 524288 | NAT 맵 최대 엔트리 수 |
| `--bpf-policy-map-max` | 65536 | 정책 맵 최대 엔트리 수 |

---

## Helm 배포

### 차트 위치

```
install/kubernetes/cilium/
├── Chart.yaml           # 차트 메타데이터
├── values.yaml          # 기본 설정값 (2000+ 줄)
├── templates/
│   ├── cilium-agent/    # DaemonSet, ConfigMap, RBAC
│   ├── cilium-operator/ # Deployment, ConfigMap, RBAC
│   ├── hubble/          # Hubble 관련 리소스
│   ├── hubble-relay/    # Relay Deployment
│   ├── hubble-ui/       # UI Deployment
│   └── clustermesh-apiserver/  # ClusterMesh 리소스
└── crds/                # CRD 정의
```

### 기본 설치

```bash
helm repo add cilium https://helm.cilium.io/
helm install cilium cilium/cilium \
  --namespace kube-system \
  --set ipam.mode=kubernetes
```

### 프로덕션 권장 설정

```yaml
# values-production.yaml
kubeProxyReplacement: true
k8sServiceHost: <API_SERVER_IP>
k8sServicePort: <API_SERVER_PORT>

hubble:
  enabled: true
  relay:
    enabled: true
  ui:
    enabled: true

bpf:
  masquerade: true
  lbAlgorithm: maglev

encryption:
  enabled: true
  type: wireguard

operator:
  replicas: 2  # HA 구성
```

---

## 디버깅 도구

### cilium-dbg (디버그 CLI)

daemon의 UNIX 소켓을 통해 상태를 조회한다.

```bash
# Endpoint 목록
cilium-dbg endpoint list

# 특정 Endpoint 상태
cilium-dbg endpoint get <id>

# BPF 맵 내용 조회
cilium-dbg bpf ct list global
cilium-dbg bpf policy get <endpoint-id>
cilium-dbg bpf lb list

# Identity 조회
cilium-dbg identity list

# 서비스 목록
cilium-dbg service list

# 상태 확인
cilium-dbg status --verbose

# 정책 확인
cilium-dbg policy get

# IP 캐시 확인
cilium-dbg bpf ipcache list
```

### hubble (관측 CLI)

```bash
# 실시간 Flow 관찰
hubble observe

# 특정 Pod의 트래픽
hubble observe --pod <namespace>/<pod-name>

# 드롭된 패킷만 필터
hubble observe --verdict DROPPED

# 특정 포트 필터
hubble observe --port 80 --protocol TCP

# JSON 출력
hubble observe -o json

# Flow 통계
hubble observe --last 1000 -o compact
```

### 로그 확인

```bash
# daemon 로그
kubectl -n kube-system logs -l k8s-app=cilium --tail=100

# operator 로그
kubectl -n kube-system logs -l name=cilium-operator --tail=100

# 디버그 모드 활성화 (런타임)
cilium-dbg config DebugEnabled=true
```

---

## 트러블슈팅 가이드

### 문제: Pod 간 통신 불가

```
1. Endpoint 상태 확인
   $ cilium-dbg endpoint list
   → state가 "ready"인지 확인

2. Identity 확인
   $ cilium-dbg identity list
   → Pod의 라벨에 맞는 Identity가 할당되었는지

3. 정책 확인
   $ cilium-dbg bpf policy get <endpoint-id>
   → 소스 Identity가 허용 목록에 있는지

4. CT 엔트리 확인
   $ cilium-dbg bpf ct list global | grep <dst-ip>
   → 연결 추적 엔트리 존재 여부

5. Hubble로 드롭 원인 확인
   $ hubble observe --pod <pod> --verdict DROPPED
   → drop_reason 확인
```

### 문제: 서비스 접근 불가

```
1. 서비스 맵 확인
   $ cilium-dbg service list
   → 서비스 Frontend → Backend 매핑 존재 여부

2. 백엔드 상태 확인
   $ cilium-dbg bpf lb list
   → 백엔드가 활성 상태인지

3. kube-proxy-replacement 모드 확인
   $ cilium-dbg status | grep KubeProxyReplacement
```

### 문제: 높은 패킷 드롭률

```
1. 드롭 원인 통계 확인
   $ cilium-dbg metrics list | grep drop

2. BPF 맵 크기 확인 (오버플로우 가능)
   $ cilium-dbg bpf ct list global | wc -l
   → bpf-ct-global-tcp-max 대비 사용량

3. CPU/메모리 리소스 확인
   → BPF 프로그램 처리량이 리소스에 의존
```

---

## 모니터링 (Prometheus 메트릭)

Cilium daemon은 `--prometheus-serve-addr` 플래그로 Prometheus 메트릭을 노출한다.
기본값은 비활성(빈 문자열)이며, 활성화 시 일반적으로 `:9962`를 사용한다.

### 핵심 메트릭

| 메트릭 | 설명 |
|--------|------|
| `cilium_endpoint_count` | 관리 중인 Endpoint 수 |
| `cilium_policy_count` | 적용된 정책 수 |
| `cilium_bpf_map_ops_total` | BPF 맵 연산 수 |
| `cilium_drop_count_total` | 데이터패스 드롭 수 (label: reason) |
| `cilium_forward_count_total` | 데이터패스 전달 수 |
| `cilium_datapath_errors_total` | 데이터패스 에러 수 |
| `cilium_identity_count` | 할당된 Identity 수 |
| `cilium_ip_addresses` | 할당된 IP 수 |
| `cilium_k8s_client_api_calls_total` | K8s API 호출 수 |
| `cilium_kvstore_operations_duration_seconds` | etcd 연산 레이턴시 |
| `hubble_flows_processed_total` | Hubble 처리 Flow 수 |
