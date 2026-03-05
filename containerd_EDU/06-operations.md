# containerd 운영 가이드

## 1. 개요

이 문서는 containerd의 **설치, 설정, 운영, 모니터링, 트러블슈팅**에 대한 가이드이다.
프로덕션 환경에서 containerd를 안정적으로 운영하기 위한 핵심 사항을 다룬다.

---

## 2. 설치

### 2.1 바이너리 설치 (공식 릴리스)

```bash
# 1. 릴리스 아카이브 다운로드
CONTAINERD_VERSION=2.0.0
wget https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-amd64.tar.gz

# 2. 바이너리 설치
sudo tar xzf containerd-${CONTAINERD_VERSION}-linux-amd64.tar.gz -C /usr/local

# 3. 포함된 바이너리 확인
ls /usr/local/bin/containerd*
# containerd
# containerd-shim-runc-v2
# containerd-stress
# ctr
```

### 2.2 runc 설치 (OCI 런타임)

containerd는 runc를 실행하므로 별도 설치 필요:

```bash
RUNC_VERSION=1.2.0
wget https://github.com/opencontainers/runc/releases/download/v${RUNC_VERSION}/runc.amd64
sudo install -m 755 runc.amd64 /usr/local/sbin/runc
```

### 2.3 CNI 플러그인 설치 (Kubernetes 사용 시)

```bash
CNI_VERSION=1.5.0
wget https://github.com/containernetworking/plugins/releases/download/v${CNI_VERSION}/cni-plugins-linux-amd64-v${CNI_VERSION}.tgz
sudo mkdir -p /opt/cni/bin
sudo tar xzf cni-plugins-linux-amd64-v${CNI_VERSION}.tgz -C /opt/cni/bin
```

### 2.4 systemd 서비스 등록

```
소스 참조: containerd.service (프로젝트 루트)
```

```bash
# 서비스 파일 설치
sudo cp containerd.service /etc/systemd/system/

# 서비스 시작
sudo systemctl daemon-reload
sudo systemctl enable containerd
sudo systemctl start containerd

# 상태 확인
sudo systemctl status containerd
```

### 2.5 소스 빌드 설치

```bash
cd containerd/
make
sudo make install

# 개별 바이너리 빌드
make bin/containerd          # 데몬만
make bin/ctr                 # CLI만
make bin/containerd-shim-runc-v2  # Shim만
```

### 2.6 설치 요구사항

| 요구사항 | 최소 버전 | 비고 |
|---------|----------|------|
| Linux 커널 | 4.x | overlayfs snapshotter 사용 시 |
| runc | 1.1.x+ | [RUNC.md](docs/RUNC.md) 참조 |
| Go | 1.24+ | 소스 빌드 시 |
| Btrfs 커널 모듈 | 3.18+ | btrfs snapshotter 사용 시 |
| criu | 3.x+ | 체크포인트/복원 시 |

---

## 3. 설정 (config.toml)

### 3.1 기본 설정 생성

```bash
# 기본 설정 파일 생성
sudo containerd config default > /etc/containerd/config.toml

# 현재 설정 출력
containerd config dump
```

### 3.2 설정 파일 구조

```
소스 참조: cmd/containerd/server/config/config.go (Line 55~94)
```

```toml
# /etc/containerd/config.toml

# 설정 파일 버전 (현재 v3)
version = 3

# 영구 데이터 저장소 경로
root = "/var/lib/containerd"

# 임시 데이터 저장소 경로
state = "/run/containerd"

# 임시 파일 디렉토리
temp = ""

# OOM 점수 조정
oom_score = 0

# gRPC 설정
[grpc]
  address = "/run/containerd/containerd.sock"
  tcp_address = ""
  tcp_tls_ca = ""
  tcp_tls_cert = ""
  tcp_tls_key = ""
  uid = 0
  gid = 0
  max_recv_message_size = 16777216
  max_send_message_size = 16777216

# TTRPC 설정
[ttrpc]
  address = ""    # 기본: {grpc.address}.ttrpc
  uid = 0
  gid = 0

# 디버그 설정
[debug]
  address = ""    # 예: "/run/containerd/debug.sock" 또는 "localhost:6060"
  uid = 0
  gid = 0
  level = ""      # trace, debug, info, warn, error, fatal, panic
  format = ""     # text, json

# 메트릭 설정
[metrics]
  address = ""    # 예: "0.0.0.0:9100"
  grpc_histogram = false

# cgroup 설정
[cgroup]
  path = ""       # containerd 자체의 cgroup 경로

# 비활성화할 플러그인
disabled_plugins = []

# 필수 플러그인 (없으면 시작 실패)
required_plugins = []

# 타임아웃 설정
[timeouts]
  # "io.containerd.timeout.shim.cleanup" = "5s"
  # "io.containerd.timeout.shim.load" = "5s"
  # "io.containerd.timeout.shim.shutdown" = "3s"
  # "io.containerd.timeout.task.state" = "2s"

# Import 추가 설정 파일
imports = ["/etc/containerd/conf.d/*.toml"]

# 프록시 플러그인
[proxy_plugins]
  # [proxy_plugins.fuse-overlayfs]
  #   type = "snapshot"
  #   address = "/run/containerd-fuse-overlayfs.sock"

# 스트림 프로세서
[stream_processors]
  # [stream_processors.encryption]
  #   accepts = ["application/vnd.oci.image.layer.v1.tar+encrypted"]
  #   returns = "application/vnd.oci.image.layer.v1.tar"
  #   path = "ctd-decoder"
```

### 3.3 플러그인별 설정

```toml
# 플러그인 설정은 [plugins."io.containerd.xxx.v1.yyy"] 형식

# GC Scheduler 설정
[plugins."io.containerd.gc.v1.scheduler"]
  pause_threshold = 0.02
  deletion_threshold = 0
  mutation_threshold = 100
  schedule_delay = "0ms"
  startup_delay = "100ms"

# Snapshotter 설정 (overlayfs)
[plugins."io.containerd.snapshotter.v1.overlayfs"]
  root_path = ""
  # mount_options = ["index=off"]

# CRI 설정 (Kubernetes 연동)
[plugins."io.containerd.cri.v1.runtime"]
  # 기본 런타임
  [plugins."io.containerd.cri.v1.runtime".containerd.default_runtime]
    runtime_type = "io.containerd.runc.v2"

  # 이미지 설정
  [plugins."io.containerd.cri.v1.images"]
    snapshotter = "overlayfs"
    disable_snapshot_annotations = false

  # CNI 설정
  [plugins."io.containerd.cri.v1.cni"]
    bin_dir = "/opt/cni/bin"
    conf_dir = "/etc/cni/net.d"
```

### 3.4 Drop-in 설정

```
소스 참조: defaults/defaults_unix.go (Line 29)
  DefaultConfigIncludePattern = "/etc/containerd/conf.d/*.toml"
```

추가 설정 파일을 `/etc/containerd/conf.d/` 디렉토리에 배치하면 자동 로드된다:

```bash
# /etc/containerd/conf.d/cri.toml
[plugins."io.containerd.cri.v1.images"]
  snapshotter = "overlayfs"

# /etc/containerd/conf.d/debug.toml
[debug]
  level = "debug"
```

### 3.5 설정 마이그레이션

containerd v2는 config v3을 사용한다. 이전 버전에서 마이그레이션:

```bash
# v1/v2 → v3 자동 마이그레이션 확인
containerd config migrate /etc/containerd/config.toml
```

```
소스 참조: cmd/containerd/server/config/config.go (Line 46~50)
  migrations = []func(context.Context, *Config) error{
      nil,         // Version 0
      v1Migrate,   // Version 1 → 2 (플러그인 이름을 URI로 변환)
      nil,         // Version 2 → 3 (플러그인 변경만)
  }
```

### 3.6 기본 디렉토리 경로

```
소스 참조: defaults/defaults_unix.go, defaults/defaults_linux.go
```

| 경로 | 용도 | 플랫폼 |
|------|------|--------|
| `/etc/containerd/` | 설정 파일 | Linux/Unix |
| `/etc/containerd/config.toml` | 메인 설정 | Linux/Unix |
| `/etc/containerd/conf.d/` | Drop-in 설정 | Linux/Unix |
| `/var/lib/containerd/` | 영구 데이터 (Root) | Linux/Unix |
| `/run/containerd/` | 임시 데이터 (State) | Linux |
| `/var/run/containerd/` | 임시 데이터 (State) | macOS/FreeBSD |
| `/run/containerd/containerd.sock` | gRPC 소켓 | Linux |
| `/run/containerd/containerd.sock.ttrpc` | TTRPC 소켓 | Linux |

---

## 4. Kubernetes CRI 연동

### 4.1 kubelet 설정

Kubernetes 1.24+에서 containerd를 CRI 런타임으로 사용:

```yaml
# /var/lib/kubelet/config.yaml (또는 kubelet 플래그)
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
containerRuntimeEndpoint: unix:///run/containerd/containerd.sock
```

### 4.2 CRI 플러그인 설정

```toml
# /etc/containerd/config.toml

version = 3

[plugins."io.containerd.cri.v1.runtime"]
  # Sandbox 이미지 (pause 컨테이너)
  sandbox_image = "registry.k8s.io/pause:3.10"

  # 런타임 설정
  [plugins."io.containerd.cri.v1.runtime".containerd]
    default_runtime_name = "runc"
    [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runc]
      runtime_type = "io.containerd.runc.v2"
      [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runc.options]
        SystemdCgroup = true

[plugins."io.containerd.cri.v1.cni"]
  bin_dir = "/opt/cni/bin"
  conf_dir = "/etc/cni/net.d"
```

### 4.3 CRI 동작 확인

```bash
# crictl 설치 (Kubernetes CRI 디버그 도구)
VERSION="v1.31.0"
wget https://github.com/kubernetes-sigs/cri-tools/releases/download/${VERSION}/crictl-${VERSION}-linux-amd64.tar.gz
sudo tar xzf crictl-${VERSION}-linux-amd64.tar.gz -C /usr/local/bin

# crictl 설정
cat <<EOF | sudo tee /etc/crictl.yaml
runtime-endpoint: unix:///run/containerd/containerd.sock
image-endpoint: unix:///run/containerd/containerd.sock
timeout: 10
debug: false
EOF

# CRI 동작 확인
sudo crictl info
sudo crictl images
sudo crictl ps -a
sudo crictl pods
```

### 4.4 네임스페이스 확인

```bash
# containerd 네임스페이스 확인
sudo ctr namespaces list
# NAME    LABELS
# default
# k8s.io          ← Kubernetes 네임스페이스
# moby            ← Docker 네임스페이스 (Docker 사용 시)

# k8s.io 네임스페이스의 컨테이너 확인
sudo ctr -n k8s.io containers list
sudo ctr -n k8s.io images list
```

---

## 5. ctr CLI 사용법

ctr은 containerd의 **디버깅/관리용 CLI**이다. 프로덕션 사용보다는 디버깅 목적이다.

### 5.1 기본 명령어

| 명령어 | 설명 |
|--------|------|
| `ctr version` | 버전 확인 |
| `ctr namespaces list` | 네임스페이스 목록 |
| `ctr images list` | 이미지 목록 |
| `ctr images pull <ref>` | 이미지 Pull |
| `ctr images push <ref>` | 이미지 Push |
| `ctr containers list` | 컨테이너 목록 |
| `ctr containers create <image> <id>` | 컨테이너 생성 |
| `ctr containers delete <id>` | 컨테이너 삭제 |
| `ctr tasks list` | 태스크 목록 |
| `ctr tasks start <id>` | 태스크 시작 |
| `ctr tasks kill <id>` | 태스크 종료 |
| `ctr tasks delete <id>` | 태스크 삭제 |
| `ctr content list` | 콘텐츠 목록 |
| `ctr snapshots list` | 스냅샷 목록 |
| `ctr leases list` | 리스 목록 |
| `ctr plugins list` | 플러그인 목록 |
| `ctr events` | 이벤트 스트림 |

### 5.2 이미지 관리 예시

```bash
# 이미지 Pull
sudo ctr images pull docker.io/library/nginx:latest

# 이미지 목록
sudo ctr images list

# 이미지 태그
sudo ctr images tag docker.io/library/nginx:latest myregistry/nginx:v1

# 이미지 삭제
sudo ctr images remove docker.io/library/nginx:latest

# 이미지 내보내기/가져오기
sudo ctr images export nginx.tar docker.io/library/nginx:latest
sudo ctr images import nginx.tar
```

### 5.3 컨테이너 실행 예시

```bash
# 컨테이너 생성 + 실행 (일회성)
sudo ctr run -t --rm docker.io/library/alpine:latest my-alpine /bin/sh

# 백그라운드 실행
sudo ctr run -d docker.io/library/nginx:latest my-nginx

# 태스크 확인
sudo ctr tasks list

# exec (추가 프로세스 실행)
sudo ctr tasks exec --exec-id bash-1 -t my-nginx /bin/bash

# 태스크 종료/삭제
sudo ctr tasks kill my-nginx
sudo ctr tasks delete my-nginx
sudo ctr containers delete my-nginx
```

### 5.4 디버깅 명령어

```bash
# 플러그인 목록 확인
sudo ctr plugins list
# TYPE                                  ID             PLATFORMS   STATUS
# io.containerd.content.v1              content        -           ok
# io.containerd.snapshotter.v1          overlayfs      linux/amd64 ok
# io.containerd.metadata.v1             bolt           -           ok
# io.containerd.gc.v1                   scheduler      -           ok
# ...

# 이벤트 실시간 모니터링
sudo ctr events

# 특정 네임스페이스
sudo ctr -n k8s.io tasks list
sudo ctr -n k8s.io containers list
```

---

## 6. 모니터링

### 6.1 Prometheus 메트릭

containerd는 Prometheus 형식의 메트릭을 노출한다.

```toml
# config.toml에서 메트릭 활성화
[metrics]
  address = "0.0.0.0:9100"
  grpc_histogram = true     # gRPC 요청 지연 히스토그램
```

```bash
# 메트릭 확인
curl http://localhost:9100/v1/metrics
```

### 6.2 주요 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `containerd_state_sandbox_count` | Gauge | 활성 샌드박스 수 |
| `containerd_state_sandbox_active` | Gauge | 실행 중 샌드박스 수 |
| `grpc_server_handled_total` | Counter | gRPC 요청 처리 총 수 |
| `grpc_server_handling_seconds` | Histogram | gRPC 요청 처리 시간 |
| `containerd_gc_sweep_duration_seconds` | Histogram | GC sweep 소요 시간 |
| `containerd_gc_mark_duration_seconds` | Histogram | GC mark 소요 시간 |
| `containerd_gc_cleaned_content_count` | Counter | GC로 삭제된 콘텐츠 수 |
| `containerd_gc_cleaned_snapshot_count` | Counter | GC로 삭제된 스냅샷 수 |
| `process_resident_memory_bytes` | Gauge | containerd 메모리 사용량 |
| `process_cpu_seconds_total` | Counter | containerd CPU 사용 시간 |

### 6.3 Grafana 대시보드

Prometheus 메트릭을 수집하여 Grafana에서 시각화:

```
권장 패널:
1. gRPC 요청 레이트 (grpc_server_handled_total)
2. gRPC 지연 시간 P50/P90/P99 (grpc_server_handling_seconds)
3. GC 실행 빈도 및 소요 시간
4. 메모리/CPU 사용량
5. 활성 샌드박스/컨테이너 수
```

### 6.4 pprof 디버그 프로파일링

```toml
# config.toml에서 디버그 엔드포인트 활성화
[debug]
  address = "localhost:6060"
  level = "debug"
```

```bash
# CPU 프로파일
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30

# 힙 메모리 프로파일
go tool pprof http://localhost:6060/debug/pprof/heap

# 고루틴 덤프
curl http://localhost:6060/debug/pprof/goroutine?debug=1

# 뮤텍스 프로파일
go tool pprof http://localhost:6060/debug/pprof/mutex

# 블록 프로파일
go tool pprof http://localhost:6060/debug/pprof/block

# expvar (실시간 변수)
curl http://localhost:6060/debug/vars
```

---

## 7. 트러블슈팅

### 7.1 일반 진단 절차

```bash
# 1. containerd 데몬 상태 확인
sudo systemctl status containerd
sudo journalctl -u containerd -f    # 실시간 로그

# 2. 소켓 접근 확인
ls -la /run/containerd/containerd.sock

# 3. 버전 확인
containerd --version
ctr version

# 4. 플러그인 상태 확인
sudo ctr plugins list | grep -v ok   # 에러/건너뜀 플러그인

# 5. 네임스페이스 확인
sudo ctr namespaces list
```

### 7.2 로그 레벨 조정

```bash
# 방법 1: config.toml
[debug]
  level = "debug"    # trace, debug, info, warn, error, fatal, panic

# 방법 2: 커맨드라인
sudo containerd --log-level debug

# 방법 3: systemd 오버라이드
sudo systemctl edit containerd
# [Service]
# ExecStart=
# ExecStart=/usr/local/bin/containerd --log-level debug
```

### 7.3 자주 발생하는 문제

**문제: containerd 시작 실패 - "failed to open boltdb"**

```
원인: 다른 containerd 인스턴스가 이미 BoltDB를 잠금
해결:
  sudo systemctl stop containerd
  # 기존 프로세스 확인
  sudo ps aux | grep containerd
  # 잠금 해제 후 재시작
  sudo systemctl start containerd
```

**문제: "context deadline exceeded" (이미지 Pull)**

```
원인: 레지스트리 연결 실패 또는 네트워크 문제
진단:
  sudo ctr images pull --debug docker.io/library/alpine:latest
해결:
  # 프록시 설정 확인
  export HTTP_PROXY=http://proxy:8080
  export HTTPS_PROXY=http://proxy:8080
  # 또는 systemd 환경변수
  sudo systemctl edit containerd
  # [Service]
  # Environment="HTTP_PROXY=http://proxy:8080"
  # Environment="HTTPS_PROXY=http://proxy:8080"
```

**문제: "no such file or directory" (shim)**

```
원인: containerd-shim-runc-v2 바이너리가 PATH에 없음
해결:
  which containerd-shim-runc-v2
  # 없으면 설치
  sudo cp containerd-shim-runc-v2 /usr/local/bin/
```

**문제: overlayfs 마운트 실패**

```
원인: 커널이 overlayfs를 지원하지 않거나 파일시스템이 호환되지 않음
진단:
  modprobe overlay
  cat /proc/filesystems | grep overlay
해결:
  # native snapshotter로 변경
  [plugins."io.containerd.cri.v1.images"]
    snapshotter = "native"
```

**문제: CRI 연결 실패 (kubelet → containerd)**

```
원인: containerd CRI 플러그인이 비활성화되었거나 소켓 경로 불일치
진단:
  sudo crictl info
  sudo ctr plugins list | grep cri
해결:
  # CRI 플러그인이 disabled_plugins에 포함되지 않았는지 확인
  # config.toml의 disabled_plugins에서 "io.containerd.cri.v1" 제거
```

### 7.4 데이터 정리

```bash
# 미사용 이미지 삭제
sudo ctr images list -q | xargs -I{} sudo ctr images remove {}

# 미사용 콘텐츠 확인
sudo ctr content list

# 미사용 스냅샷 확인
sudo ctr snapshots list

# GC 강제 실행 (containerd 재시작)
sudo systemctl restart containerd
```

---

## 8. 보안

### 8.1 소켓 권한

```bash
# containerd 소켓은 root 전용
ls -la /run/containerd/containerd.sock
# srw-rw---- 1 root root ... /run/containerd/containerd.sock

# 특정 그룹 접근 허용
[grpc]
  address = "/run/containerd/containerd.sock"
  uid = 0
  gid = 1000    # docker 그룹 등
```

### 8.2 TLS 설정 (원격 접근)

```toml
[grpc]
  tcp_address = "0.0.0.0:10010"
  tcp_tls_cert = "/etc/containerd/tls/server.crt"
  tcp_tls_key = "/etc/containerd/tls/server.key"
  tcp_tls_ca = "/etc/containerd/tls/ca.crt"
```

### 8.3 Rootless 모드

containerd는 rootless 모드를 지원한다 (user namespace 활용):

```bash
# rootless containerd 실행
containerd-rootless.sh

# 소켓: $XDG_RUNTIME_DIR/containerd/containerd.sock
```

### 8.4 AppArmor / Seccomp

```toml
# CRI 플러그인에서 AppArmor/Seccomp 기본 프로필 설정
[plugins."io.containerd.cri.v1.runtime"]
  enable_selinux = false
  # Seccomp 기본 프로필
  # 컨테이너 Spec에서 개별 설정 가능
```

---

## 9. 업그레이드 / 마이그레이션

### 9.1 containerd v1 → v2 마이그레이션

containerd v2.0은 **메이저 업그레이드**이다.

| 변경사항 | v1 | v2 |
|---------|-----|-----|
| Config 버전 | v1/v2 | v3 |
| 모듈 경로 | `github.com/containerd/containerd` | `github.com/containerd/containerd/v2` |
| CRI 플러그인 | `io.containerd.grpc.v1.cri` | `io.containerd.cri.v1.*` |
| Runtime v1 | 지원 | 제거됨 |
| Shim v1 | 지원 | 제거됨 |

### 9.2 마이그레이션 절차

```bash
# 1. 설정 마이그레이션
containerd config migrate /etc/containerd/config.toml > /etc/containerd/config.toml.v3
# 검토 후 적용
mv /etc/containerd/config.toml.v3 /etc/containerd/config.toml

# 2. 새 바이너리 설치
sudo systemctl stop containerd
sudo cp containerd-v2.0.0/* /usr/local/bin/

# 3. 재시작
sudo systemctl start containerd

# 4. 확인
containerd --version
sudo ctr plugins list
```

### 9.3 롤백 절차

```bash
# 새 바이너리로 문제 발생 시
sudo systemctl stop containerd

# 이전 바이너리 복원
sudo cp /backup/containerd-v1/* /usr/local/bin/

# 이전 설정 복원
sudo cp /backup/config.toml /etc/containerd/config.toml

# 재시작
sudo systemctl start containerd
```

### 9.4 데이터 호환성

```
containerd 데이터 디렉토리:

/var/lib/containerd/
├── io.containerd.content.v1/     ← 콘텐츠 (호환됨)
├── io.containerd.metadata.v1/    ← 메타데이터 BoltDB (마이그레이션 필요할 수 있음)
├── io.containerd.snapshotter.v1.overlayfs/  ← 스냅샷 (호환됨)
└── ...

주의:
- BoltDB 스키마는 자동 마이그레이션됨 (core/metadata/migrations.go)
- 다운그레이드 시 스키마 비호환 가능 → 백업 필수
```

---

## 10. 운영 체크리스트

### 10.1 배포 전 확인

```
[ ] containerd 바이너리 설치 확인
[ ] runc 바이너리 설치 확인
[ ] CNI 플러그인 설치 확인 (Kubernetes 사용 시)
[ ] config.toml 설정 검토
[ ] CRI 플러그인 활성화 확인 (Kubernetes 사용 시)
[ ] systemd 서비스 등록 확인
[ ] 소켓 권한 확인
[ ] 메트릭 엔드포인트 설정 (모니터링)
[ ] 로그 레벨 설정 (프로덕션: info)
[ ] 디스크 공간 확인 (/var/lib/containerd)
```

### 10.2 정기 운영 확인

```
[ ] containerd 데몬 상태 (systemctl status)
[ ] 디스크 사용량 모니터링 (/var/lib/containerd)
[ ] GC 정상 동작 확인 (메트릭)
[ ] 로그 에러/경고 확인 (journalctl)
[ ] 메모리/CPU 사용량 모니터링
[ ] 스냅샷 누적 확인 (ctr snapshots list)
[ ] 미참조 콘텐츠 확인 (ctr content list)
```

### 10.3 장애 대응 절차

```
1. 증상 확인
   → systemctl status containerd
   → journalctl -u containerd --since "5 minutes ago"

2. 로그 레벨 올리기
   → debug 레벨로 재시작

3. 리소스 확인
   → 디스크, 메모리, CPU, 파일 디스크립터

4. 플러그인 상태
   → ctr plugins list

5. 네트워크 확인 (이미지 Pull 문제)
   → DNS, 프록시, 레지스트리 연결

6. containerd 재시작
   → systemctl restart containerd
   → 컨테이너는 Shim에 의해 계속 실행됨

7. 에스컬레이션
   → pprof 수집, goroutine 덤프
   → GitHub Issue 또는 CNCF Slack (#containerd)
```
