# 06. 운영 가이드 (Operations)

## 빌드

### 사전 요구사항

| 도구 | 최소 버전 | 용도 |
|------|----------|------|
| **Go** | 1.24+ (toolchain 1.25.6) | 컴파일러 |
| **Make** | 3.81+ | 빌드 자동화 |
| **Git** | 2.0+ | 버전 관리 |
| **Docker** (선택) | 20.10+ | 컨테이너 이미지 빌드 |

### 빌드 명령어

```bash
# 기본 빌드 (현재 플랫폼)
make hubble

# 빌드 결과물 확인
ls -la hubble

# 크로스 플랫폼 릴리스 빌드
make local-release
# → hubble-darwin-amd64, hubble-darwin-arm64
# → hubble-linux-amd64, hubble-linux-arm64
# → hubble-windows-amd64.exe, hubble-windows-arm64.exe

# 시스템에 설치 (/usr/local/bin)
make install
```

### 빌드 내부 동작

```bash
# 실제 빌드 명령 (Makefile에서 추출)
CGO_ENABLED=0 go build \
    -ldflags "-w -s \
        -X 'github.com/cilium/cilium/hubble/pkg.GitBranch=$(git rev-parse --abbrev-ref HEAD)' \
        -X 'github.com/cilium/cilium/hubble/pkg.GitHash=$(git rev-parse --short HEAD)' \
        -X 'github.com/cilium/cilium/hubble/pkg.Version=v1.18.6'" \
    -o hubble .
```

**왜 `CGO_ENABLED=0`인가?**
- 순수 Go 바이너리 생성 → C 라이브러리 의존성 없음
- 정적 링킹으로 어떤 리눅스 배포판에서도 동작
- Alpine 기반 Docker 이미지에서 libc 없이 실행 가능

**왜 `-w -s` LDFLAGS인가?**
- `-w`: DWARF 디버그 정보 제거 → 바이너리 크기 감소
- `-s`: 심볼 테이블 제거 → 추가 크기 감소
- 릴리스 바이너리에 불필요한 디버그 정보를 제거하여 배포 크기 최적화

### Docker 이미지 빌드

```bash
# Docker 이미지 빌드
make image

# 결과: quay.io/cilium/hubble:v1.18.6
```

**Dockerfile 구조 (멀티 스테이지):**

```
Stage 1: golang:1.25.6-alpine (빌드)
  → go build → hubble 바이너리

Stage 2: scratch 또는 minimal (실행)
  → hubble 바이너리만 복사
  → 최소 이미지 크기
```

### 테스트

```bash
# 단위 테스트 실행 (커버리지 포함)
make test

# 벤치마크 실행
make bench
```

---

## 설정

### 설정 우선순위 (높은 순)

```
1. CLI 플래그          --server relay:4245
2. 환경 변수           HUBBLE_SERVER=relay:4245
3. 설정 파일           config.yaml → server: relay:4245
4. 기본값              localhost:4245
```

### 환경 변수 규칙

CLI 플래그를 환경 변수로 변환하는 규칙:
- 접두어: `HUBBLE_`
- 대시(`-`) → 밑줄(`_`)
- 대문자 변환

| 플래그 | 환경 변수 |
|--------|----------|
| `--server` | `HUBBLE_SERVER` |
| `--tls-allow-insecure` | `HUBBLE_TLS_ALLOW_INSECURE` |
| `--port-forward` | `HUBBLE_PORT_FORWARD` |
| `--kube-context` | `HUBBLE_KUBE_CONTEXT` |

### 설정 파일

설정 파일 탐색 경로 (순서):
1. `./config.yaml` (현재 디렉토리)
2. `$XDG_CONFIG_HOME/hubble/config.yaml`
3. `~/.hubble/config.yaml`

**설정 파일 예시 (`~/.hubble/config.yaml`):**

```yaml
# Hubble 서버 주소
server: relay.hubble.svc.cluster.local:4245

# 연결 타임아웃
timeout: 10s
request-timeout: 30s

# TLS 설정
tls: true
tls-allow-insecure: false
tls-ca-cert-files: /etc/hubble/ca.crt
tls-server-name: relay.hubble.io

# Kubernetes 설정
kube-context: production-cluster
kube-namespace: kube-system

# 디버그 모드
debug: false
```

### 설정 관리 CLI

```bash
# 현재 설정 전체 보기
hubble config view

# 특정 설정값 조회
hubble config get server

# 설정값 변경 (설정 파일에 저장)
hubble config set server relay:4245

# 설정 초기화
hubble config reset server
```

### 호환성 옵션

`HUBBLE_COMPAT` 환경 변수로 하위 호환성 동작을 제어합니다 (`GODEBUG`과 유사한 방식):

```bash
export HUBBLE_COMPAT="option1=value1,option2=value2"
```

---

## 배포

### Kubernetes 설치

Hubble은 Cilium의 일부로 배포됩니다:

```bash
# Helm으로 Cilium + Hubble 설치
helm install cilium cilium/cilium \
    --namespace kube-system \
    --set hubble.enabled=true \
    --set hubble.relay.enabled=true \
    --set hubble.ui.enabled=true

# Hubble CLI 설치 (로컬)
HUBBLE_VERSION=$(curl -s https://raw.githubusercontent.com/cilium/hubble/main/stable.txt)
HUBBLE_ARCH=amd64
curl -L --fail --remote-name-all \
    https://github.com/cilium/hubble/releases/download/$HUBBLE_VERSION/hubble-linux-${HUBBLE_ARCH}.tar.gz
tar xzvf hubble-linux-${HUBBLE_ARCH}.tar.gz
sudo mv hubble /usr/local/bin
```

### Hubble 활성화 Helm Values

```yaml
# .github/cilium-values.yaml 참고
hubble:
  enabled: true
  metrics:
    enabled:
      - dns
      - drop
      - tcp
      - flow
      - icmp
      - http
    serviceMonitor:
      enabled: true
  relay:
    enabled: true
    replicas: 1
  ui:
    enabled: true
    replicas: 1
```

### 연결 확인

```bash
# 1. port-forward로 직접 연결
hubble observe --port-forward --last 5

# 2. Relay를 통한 연결
kubectl port-forward -n kube-system deploy/hubble-relay 4245:4245 &
hubble observe --server localhost:4245 --last 5

# 3. 서버 상태 확인
hubble status --port-forward
```

---

## 트러블슈팅

### 연결 문제

**증상: "failed to connect to hubble server"**

```bash
# 1. Cilium Pod이 Running인지 확인
kubectl get pods -n kube-system -l k8s-app=cilium

# 2. Hubble이 활성화되었는지 확인
kubectl exec -n kube-system ds/cilium -- cilium status | grep Hubble

# 3. Hubble Relay가 실행 중인지 확인
kubectl get pods -n kube-system -l k8s-app=hubble-relay

# 4. Port-forward로 직접 연결 테스트
hubble observe --port-forward --last 1
```

**증상: "TLS handshake error"**

```bash
# CA 인증서 확인
kubectl get secret -n kube-system hubble-ca-secret -o jsonpath='{.data.ca\.crt}' | base64 -d

# TLS 비활성화로 테스트 (개발 환경만)
hubble observe --tls=false --server localhost:4245

# 인증서 검증 생략 (디버깅용, 프로덕션 금지)
hubble observe --tls --tls-allow-insecure
```

### 데이터 문제

**증상: "no flows found"**

```bash
# 1. 서버 상태에서 플로우 수 확인
hubble status --port-forward
# → num_flows: 0 이면 트래픽이 없거나 Hubble이 비활성화

# 2. 필터가 너무 엄격한지 확인 (필터 없이 테스트)
hubble observe --port-forward --last 10

# 3. 트래픽 생성하여 테스트
kubectl run curl --image=curlimages/curl --rm -it -- curl http://httpbin.org/get
```

**증상: "lost events" 경고**

```bash
# Ring buffer 크기가 너무 작으면 이벤트 유실 발생
# Cilium 설정에서 버퍼 크기 조정
helm upgrade cilium cilium/cilium \
    --set hubble.eventBufferCapacity=65535
```

### 성능 문제

**증상: 높은 메모리 사용량**

```bash
# Ring buffer 용량 확인
hubble status --port-forward
# → max_flows 값이 메모리 사용량에 비례

# field-mask로 응답 크기 줄이기
hubble observe --follow --field-mask source.pod_name,destination.pod_name,verdict
```

**증상: CLI 응답이 느림**

```bash
# 타임아웃 늘리기
hubble observe --timeout 30s --request-timeout 60s

# 디버그 모드로 병목 확인
hubble observe --debug --last 5
```

---

## CI/CD 파이프라인

### GitHub Actions 워크플로우

```
.github/workflows/
├── ci.yml          # PR/Push 시 실행
│   ├── 단위 테스트
│   ├── 벤더 일관성 검사
│   └── 코드 린팅
└── release.yml     # 태그 푸시 시 실행
    ├── 크로스 플랫폼 빌드
    ├── Docker 이미지 빌드 & 푸시
    └── GitHub Release 생성
```

### 테스트 환경

CI에서는 `kind` (Kubernetes in Docker)를 사용합니다:

```yaml
# .github/kind-config.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
networking:
  disableDefaultCNI: true    # Cilium을 CNI로 사용
  kubeProxyMode: "none"      # Cilium의 kube-proxy 대체 기능 사용
```

---

## 릴리스 프로세스

1. **버전 결정**: Cilium 종속성 버전과 동기화 (`go list -f {{.Version}} -m github.com/cilium/cilium`)
2. **CHANGELOG 업데이트**: 변경사항 기록
3. **태그 생성**: `git tag v1.18.6`
4. **자동 빌드**: GitHub Actions가 크로스 플랫폼 바이너리 + Docker 이미지 빌드
5. **릴리스 발행**: GitHub Release에 바이너리 업로드, quay.io에 이미지 푸시
6. **stable.txt 업데이트**: 안정 버전 정보 갱신

---

## 직접 실행해보기 (PoC)

| PoC | 실행 | 학습 내용 |
|-----|------|----------|
| [poc-config-priority](poc-config-priority/) | `MINI_HUBBLE_SERVER=env:4245 go run main.go --server=flag:4245` | Flag > Env > File > Default 우선순위 |
| [poc-prometheus-metrics](poc-prometheus-metrics/) | `cd poc-prometheus-metrics && go run main.go` | Counter/Gauge/Histogram, /metrics 텍스트 형식 |
