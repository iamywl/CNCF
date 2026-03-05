# 06. Kubernetes 운영

## 개요

Kubernetes 클러스터의 배포, 설정, 모니터링, 트러블슈팅 방법을 설명한다.

## 배포 방식

### kubeadm

Kubernetes 공식 클러스터 부트스트래핑 도구.

```bash
# Control Plane 초기화
kubeadm init --pod-network-cidr=10.244.0.0/16

# Worker Node 조인
kubeadm join <control-plane-ip>:6443 --token <token> \
  --discovery-token-ca-cert-hash sha256:<hash>

# 업그레이드
kubeadm upgrade plan
kubeadm upgrade apply v1.31.0
```

### 컴포넌트별 실행 옵션

```bash
# kube-apiserver 주요 플래그
kube-apiserver \
  --etcd-servers=https://127.0.0.1:2379 \
  --service-cluster-ip-range=10.96.0.0/12 \
  --service-account-key-file=/etc/kubernetes/pki/sa.pub \
  --service-account-signing-key-file=/etc/kubernetes/pki/sa.key \
  --authorization-mode=Node,RBAC \
  --enable-admission-plugins=NodeRestriction \
  --secure-port=6443

# kube-controller-manager 주요 플래그
kube-controller-manager \
  --kubeconfig=/etc/kubernetes/controller-manager.conf \
  --controllers=*,bootstrapsigner,tokencleaner \
  --leader-elect=true \
  --use-service-account-credentials=true

# kube-scheduler 주요 플래그
kube-scheduler \
  --kubeconfig=/etc/kubernetes/scheduler.conf \
  --leader-elect=true

# kubelet 주요 플래그
kubelet \
  --kubeconfig=/etc/kubernetes/kubelet.conf \
  --container-runtime-endpoint=unix:///run/containerd/containerd.sock \
  --pod-infra-container-image=registry.k8s.io/pause:3.10
```

## 설정 파일

### KubeletConfiguration

```yaml
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
clusterDNS:
  - 10.96.0.10
clusterDomain: cluster.local
cgroupDriver: systemd
containerRuntimeEndpoint: unix:///run/containerd/containerd.sock
evictionHard:
  memory.available: "100Mi"
  nodefs.available: "10%"
  imagefs.available: "15%"
maxPods: 110
syncFrequency: 1m
```

### KubeSchedulerConfiguration

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
profiles:
  - schedulerName: default-scheduler
    plugins:
      score:
        enabled:
          - name: NodeResourcesFit
            weight: 1
          - name: InterPodAffinity
            weight: 1
```

### KubeControllerManagerConfiguration

```yaml
apiVersion: kubecontrollermanager.config.k8s.io/v1alpha1
kind: KubeControllerManagerConfiguration
generic:
  controllers:
    - "*"
  leaderElection:
    leaderElect: true
    leaseDuration: 15s
    renewDeadline: 10s
    retryPeriod: 2s
```

## 모니터링

### Prometheus 메트릭 엔드포인트

모든 컴포넌트는 `/metrics` 엔드포인트를 제공한다.

| 컴포넌트 | 포트 | 엔드포인트 |
|----------|------|-----------|
| kube-apiserver | 6443 | `/metrics` |
| kube-controller-manager | 10257 | `/metrics` |
| kube-scheduler | 10259 | `/metrics` |
| kubelet | 10250 | `/metrics`, `/metrics/cadvisor` |
| kube-proxy | 10249 | `/metrics` |

### 핵심 메트릭

#### API Server

```
# 요청 지연 시간
apiserver_request_duration_seconds_bucket{verb, resource, subresource}

# 요청 수
apiserver_request_total{verb, resource, code}

# Watch 수
apiserver_longrunning_requests{verb="WATCH"}

# etcd 요청 지연
etcd_request_duration_seconds_bucket{operation, type}

# 인플라이트 요청
apiserver_current_inflight_requests{request_kind}
```

#### Scheduler

```
# 스케줄링 시도 수
scheduler_schedule_attempts_total{result, profile}

# 스케줄링 지연
scheduler_scheduling_attempt_duration_seconds_bucket{result, profile}

# 큐 대기 시간
scheduler_pending_pods{queue}

# Preemption 횟수
scheduler_preemption_victims
```

#### Controller Manager

```
# 워크 큐 깊이
workqueue_depth{name}

# 워크 큐 지연
workqueue_queue_duration_seconds_bucket{name}

# 재시도 횟수
workqueue_retries_total{name}

# 처리 속도
workqueue_work_duration_seconds_bucket{name}
```

#### Kubelet

```
# 실행 중인 Pod/컨테이너 수
kubelet_running_pods
kubelet_running_containers{container_state}

# Pod 시작 지연
kubelet_pod_start_duration_seconds_bucket

# PLEG relist 지연
kubelet_pleg_relist_duration_seconds_bucket

# 볼륨 작업
storage_operation_duration_seconds_bucket{operation_name, volume_plugin}
```

### 헬스 체크 엔드포인트

```
# 전반적 건강 상태
/healthz

# 세부 건강 상태
/healthz?verbose=true

# Livez (개별 체크)
/livez
/livez/etcd
/livez/poststarthook/...

# Readyz (준비 완료)
/readyz
/readyz/informer-sync
/readyz/shutdown
```

## 트러블슈팅

### Pod가 Pending 상태일 때

```bash
# 1. Pod 이벤트 확인
kubectl describe pod <name>

# 2. 스케줄러 로그 확인
kubectl logs -n kube-system kube-scheduler-<node>

# 3. 일반적 원인
#    - Insufficient CPU/Memory → 노드 리소스 확인
#    - NodeSelector/Affinity 불일치 → 라벨 확인
#    - Taint/Toleration → 노드 Taint 확인
#    - PVC 바인딩 대기 → PV 가용성 확인
```

### Pod가 CrashLoopBackOff일 때

```bash
# 1. 컨테이너 로그 확인
kubectl logs <pod> --previous

# 2. Pod 상태 확인
kubectl get pod <name> -o yaml

# 3. 일반적 원인
#    - 애플리케이션 에러 (exit code != 0)
#    - OOMKilled (메모리 부족)
#    - 설정 오류 (ConfigMap/Secret 누락)
#    - Liveness Probe 실패
```

### Node NotReady 상태일 때

```bash
# 1. 노드 상태 확인
kubectl describe node <name>

# 2. kubelet 로그 확인
journalctl -u kubelet -f

# 3. 일반적 원인
#    - kubelet 프로세스 중지
#    - 컨테이너 런타임 장애
#    - 디스크/메모리 압박 (eviction threshold)
#    - 네트워크 단절
```

### API Server 접속 불가

```bash
# 1. API Server 프로세스 확인
kubectl get --raw /healthz

# 2. etcd 상태 확인
etcdctl endpoint health

# 3. 인증서 만료 확인
kubeadm certs check-expiration

# 4. 일반적 원인
#    - etcd 클러스터 장애
#    - 인증서 만료
#    - 리소스 부족 (OOM)
#    - 네트워크 설정 오류
```

### 네트워킹 문제

```bash
# 1. Service → Endpoint 확인
kubectl get endpoints <service-name>

# 2. kube-proxy 로그
kubectl logs -n kube-system kube-proxy-<hash>

# 3. DNS 확인
kubectl exec <pod> -- nslookup kubernetes.default

# 4. iptables 규칙 확인
iptables -t nat -L KUBE-SERVICES

# 5. CNI 플러그인 상태
ls /etc/cni/net.d/
```

## 백업과 복구

### etcd 백업

```bash
# 스냅샷 생성
etcdctl snapshot save /backup/etcd-snapshot.db \
  --endpoints=https://127.0.0.1:2379 \
  --cacert=/etc/kubernetes/pki/etcd/ca.crt \
  --cert=/etc/kubernetes/pki/etcd/server.crt \
  --key=/etc/kubernetes/pki/etcd/server.key

# 스냅샷 상태 확인
etcdctl snapshot status /backup/etcd-snapshot.db --write-out=table
```

### etcd 복구

```bash
# 1. API Server 중지
# 2. etcd 복구
etcdctl snapshot restore /backup/etcd-snapshot.db \
  --data-dir=/var/lib/etcd-restore

# 3. etcd 데이터 디렉토리 교체
mv /var/lib/etcd /var/lib/etcd.old
mv /var/lib/etcd-restore /var/lib/etcd

# 4. etcd 및 API Server 재시작
```

## 업그레이드 전략

### Control Plane 업그레이드

```
1. etcd 백업
2. kubeadm upgrade plan
3. kubeadm upgrade apply v1.31.0 (첫 번째 Control Plane)
4. kubeadm upgrade node (추가 Control Plane)
5. kubelet, kubectl 업그레이드
6. systemctl restart kubelet
```

### Worker Node 업그레이드

```
1. kubectl drain <node> --ignore-daemonsets
2. kubeadm upgrade node
3. kubelet, kubectl 업그레이드
4. systemctl restart kubelet
5. kubectl uncordon <node>
```

## 성능 튜닝

### API Server

```
--max-requests-inflight=400         # 최대 동시 요청 (비-mutating)
--max-mutating-requests-inflight=200 # 최대 동시 요청 (mutating)
--watch-cache-sizes=...             # Watch 캐시 크기
--etcd-count-metric-poll-period=1m  # etcd 메트릭 폴링 주기
```

### etcd

```
--quota-backend-bytes=8589934592    # 스토리지 쿼타 (8GB)
--auto-compaction-mode=periodic     # 자동 컴팩션
--auto-compaction-retention=1h      # 1시간 주기
```

### Kubelet

```
--max-pods=110                      # 노드당 최대 Pod 수
--kube-api-qps=50                   # API 요청 QPS
--kube-api-burst=100                # API 버스트
--serialize-image-pulls=false       # 병렬 이미지 풀
```

### Scheduler

```
--kube-api-qps=50                   # API 요청 QPS
--kube-api-burst=100                # API 버스트
# 프로파일별 노드 비율 조정 가능
```
