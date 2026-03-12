# CoreDNS 운영 가이드

## 1. 배포

### 1.1 바이너리 직접 빌드

```bash
# 소스 빌드
git clone https://github.com/coredns/coredns
cd coredns
make

# 실행
./coredns -conf Corefile
./coredns -conf Corefile -dns.port 1053   # 포트 변경
./coredns -plugins                         # 플러그인 목록 확인
./coredns -version                         # 버전 확인
```

### 1.2 Docker 배포

```bash
# 공식 이미지
docker pull coredns/coredns

# 실행
docker run -d --name coredns \
  -p 53:53/udp -p 53:53/tcp \
  -v $(pwd)/Corefile:/Corefile \
  coredns/coredns

# 커스텀 빌드 (외부 플러그인 포함)
docker build -t my-coredns .
```

### 1.3 Kubernetes 배포

CoreDNS는 Kubernetes의 기본 DNS 서버로, 일반적으로 `kube-system` 네임스페이스에 Deployment로 배포된다.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: coredns
  namespace: kube-system
  labels:
    k8s-app: kube-dns
spec:
  replicas: 2
  selector:
    matchLabels:
      k8s-app: kube-dns
  template:
    metadata:
      labels:
        k8s-app: kube-dns
    spec:
      serviceAccountName: coredns
      containers:
      - name: coredns
        image: coredns/coredns:latest
        resources:
          limits:
            memory: 170Mi
          requests:
            cpu: 100m
            memory: 70Mi
        args: ["-conf", "/etc/coredns/Corefile"]
        volumeMounts:
        - name: config-volume
          mountPath: /etc/coredns
        ports:
        - containerPort: 53
          name: dns
          protocol: UDP
        - containerPort: 53
          name: dns-tcp
          protocol: TCP
        - containerPort: 9153
          name: metrics
          protocol: TCP
        livenessProbe:
          httpGet:
            path: /health
            port: 8080
          initialDelaySeconds: 60
          timeoutSeconds: 5
        readinessProbe:
          httpGet:
            path: /ready
            port: 8181
      volumes:
      - name: config-volume
        configMap:
          name: coredns
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: coredns
  namespace: kube-system
data:
  Corefile: |
    .:53 {
        errors
        health {
            lameduck 5s
        }
        ready
        kubernetes cluster.local in-addr.arpa ip6.arpa {
            pods insecure
            fallthrough in-addr.arpa ip6.arpa
            ttl 30
        }
        prometheus :9153
        forward . /etc/resolv.conf {
            max_concurrent 1000
        }
        cache 30
        loop
        reload
        loadbalance
    }
---
apiVersion: v1
kind: Service
metadata:
  name: kube-dns
  namespace: kube-system
  labels:
    k8s-app: kube-dns
spec:
  selector:
    k8s-app: kube-dns
  clusterIP: 10.96.0.10
  ports:
  - name: dns
    port: 53
    protocol: UDP
  - name: dns-tcp
    port: 53
    protocol: TCP
  - name: metrics
    port: 9153
    protocol: TCP
```

## 2. Corefile 설정 가이드

### 2.1 기본 구조

```
[SCHEME://]ZONE[:PORT] {
    [PLUGIN] [ARGUMENTS...]
}
```

### 2.2 일반적인 설정 예제

#### 기본 포워딩 서버

```
.:53 {
    forward . 8.8.8.8 8.8.4.4
    cache 30
    log
    errors
}
```

#### 다중 Zone 설정

```
# 내부 Zone (Zone 파일 기반)
example.com:53 {
    file db.example.com
    log
    errors
}

# 나머지 도메인 포워딩
.:53 {
    forward . 8.8.8.8
    cache 30
    log
    errors
}
```

#### Kubernetes 클러스터 DNS

```
.:53 {
    errors
    health :8080
    ready :8181

    kubernetes cluster.local in-addr.arpa ip6.arpa {
        pods insecure
        fallthrough in-addr.arpa ip6.arpa
        ttl 30
    }

    prometheus :9153
    forward . /etc/resolv.conf
    cache 30
    loop
    reload
    loadbalance
}
```

#### DoT (DNS-over-TLS) 서버

```
tls://.:853 {
    tls /path/to/cert.pem /path/to/key.pem /path/to/ca.pem
    forward . 1.1.1.1 1.0.0.1
    cache 30
    log
}
```

#### DoH (DNS-over-HTTPS) 서버

```
https://.:443 {
    tls /path/to/cert.pem /path/to/key.pem
    forward . 8.8.8.8
    cache 30
    log
}
```

### 2.3 플러그인 설정 상세

#### forward

```
forward FROM TO... {
    except IGNORED_NAMES...       # 제외 도메인
    force_tcp                     # TCP 강제
    prefer_udp                    # UDP 우선
    expire DURATION               # 연결 만료 (기본 10s)
    max_fails INTEGER             # 최대 실패 수 (기본 2)
    max_concurrent INTEGER        # 최대 동시 쿼리
    health_check DURATION         # 헬스체크 간격 (기본 0.5s)
    policy random|round_robin|sequential
    tls CERT KEY CA               # TLS 설정
    tls_servername NAME           # TLS 서버 이름
}
```

#### cache

```
cache [TTL] [ZONES...] {
    success CAPACITY [TTL] [MINTTL]   # 양성 캐시 설정
    denial CAPACITY [TTL] [MINTTL]    # 음성 캐시 설정
    prefetch AMOUNT [[DURATION] [PERCENTAGE%]]
    serve_stale [DURATION] [VERIFY]   # 노화 서빙
    servfail DURATION                 # SERVFAIL 캐시 TTL
    keepttl                           # 원래 TTL 유지
}
```

#### kubernetes

```
kubernetes [ZONES...] {
    endpoint URL                      # API 서버 주소
    tls CERT KEY CACERT              # TLS 설정
    kubeconfig KUBECONFIG [CONTEXT]  # kubeconfig 파일
    namespaces NAMESPACE...          # 노출 네임스페이스
    labels EXPRESSION                # 레이블 셀렉터
    pods POD-MODE                    # disabled|insecure|verified
    endpoint_pod_names               # 엔드포인트 Pod 이름 사용
    ttl TTL                          # 응답 TTL (기본 5초)
    fallthrough [ZONES...]           # 다음 플러그인으로 전달
}
```

#### log

```
log [NAMES...] {
    class CLASS                      # success|denial|error|all
    format FORMAT                    # combined|common|{format}
}
```

### 2.4 설정 핫 리로드

`reload` 플러그인을 사용하면 Corefile 변경 시 자동으로 재로드된다.

```
.:53 {
    reload 10s          # 10초마다 Corefile 변경 확인
    forward . 8.8.8.8
}
```

또는 SIGHUP 시그널로 수동 리로드:

```bash
kill -SIGHUP $(pidof coredns)
```

## 3. 모니터링

### 3.1 Prometheus 메트릭

`prometheus` 플러그인(Corefile에서 `prometheus :9153`)을 활성화하면 `/metrics` 엔드포인트에서 메트릭을 수집할 수 있다.

#### 핵심 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `coredns_dns_requests_total` | Counter | 총 DNS 요청 수 |
| `coredns_dns_responses_total` | Counter | 총 DNS 응답 수 (rcode별) |
| `coredns_dns_request_duration_seconds` | Histogram | 요청 처리 시간 |
| `coredns_dns_request_size_bytes` | Histogram | 요청 크기 |
| `coredns_dns_response_size_bytes` | Histogram | 응답 크기 |
| `coredns_panics_total` | Counter | 패닉 복구 횟수 |

#### Cache 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `coredns_cache_hits_total` | Counter | 캐시 히트 수 (success/denial) |
| `coredns_cache_misses_total` | Counter | 캐시 미스 수 |
| `coredns_cache_requests_total` | Counter | 캐시 요청 수 |
| `coredns_cache_prefetch_total` | Counter | 프리페치 수 |
| `coredns_cache_served_stale_total` | Counter | 노화 서빙 수 |

#### Forward 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `coredns_forward_requests_total` | Counter | 포워딩 요청 수 |
| `coredns_forward_responses_total` | Counter | 포워딩 응답 수 |
| `coredns_forward_request_duration_seconds` | Histogram | 포워딩 처리 시간 |
| `coredns_forward_healthcheck_failures_total` | Counter | 헬스체크 실패 수 |
| `coredns_forward_max_concurrent_rejects_total` | Counter | 동시성 제한 거부 수 |

### 3.2 Grafana 대시보드

CoreDNS 공식 Grafana 대시보드 ID: `14981`

주요 패널:
- DNS 요청 처리량 (QPS)
- 응답 코드 분포
- 요청 처리 지연시간 (p50, p99)
- 캐시 히트율
- 업스트림 헬스 상태

### 3.3 헬스체크 엔드포인트

#### health 플러그인

```
health :8080 {
    lameduck 5s    # 종료 전 비정상 응답 시간
}
```

- `GET /health` → 200 OK (정상) 또는 503 (비정상)
- `lameduck`: graceful shutdown 시 서비스 디스커버리에서 제거될 시간

#### ready 플러그인

```
ready :8181
```

- `GET /ready` → 200 OK (모든 플러그인 준비 완료)
- K8s의 readinessProbe에 사용

### 3.4 DNSTap

```
dnstap /tmp/dnstap.sock full
```

DNSTap은 DNS 쿼리/응답을 구조화된 형식으로 외부 수집기에 전달한다.

## 4. 트러블슈팅

### 4.1 디버그 모드

```
.:53 {
    debug               # 패닉 복구 비활성화, 디버그 로깅
    log                 # 쿼리 로깅
    errors              # 에러 로깅
    forward . 8.8.8.8
}
```

### 4.2 일반적인 문제와 해결

#### 문제: SERVFAIL 응답

```bash
# 확인 사항:
# 1. 업스트림 서버 상태 확인
dig @upstream-dns example.com

# 2. CoreDNS 로그 확인
kubectl logs -n kube-system -l k8s-app=kube-dns

# 3. forward 플러그인 메트릭 확인
curl http://coredns:9153/metrics | grep forward_healthcheck
```

**원인**: 업스트림 DNS 서버 장애, 네트워크 문제, DNS 루프

#### 문제: 루프 감지 (loop detected)

```
[FATAL] plugin/loop: Loop ... detected for zone ".", see ...
```

**원인**: CoreDNS가 자기 자신에게 쿼리를 포워딩
**해결**: `forward` 디렉티브에서 CoreDNS 자신의 주소를 제외

```
.:53 {
    forward . /etc/resolv.conf  # resolv.conf에 CoreDNS 자신이 있으면 루프
    # → 직접 업스트림 지정:
    forward . 8.8.8.8
    loop    # loop 플러그인이 감지
}
```

#### 문제: 높은 지연시간

```bash
# 캐시 히트율 확인
curl -s http://coredns:9153/metrics | grep cache_hits
curl -s http://coredns:9153/metrics | grep cache_misses

# 업스트림 지연시간 확인
curl -s http://coredns:9153/metrics | grep forward_request_duration
```

**해결**:
- 캐시 TTL 증가: `cache 300`
- 프리페치 활성화: `cache 30 { prefetch 10 }`
- 업스트림 추가: `forward . 8.8.8.8 1.1.1.1`

#### 문제: K8s 서비스 해석 실패

```bash
# Pod 내에서 확인
nslookup kubernetes.default.svc.cluster.local
dig @10.96.0.10 kubernetes.default.svc.cluster.local

# CoreDNS Pod 상태 확인
kubectl get pods -n kube-system -l k8s-app=kube-dns
kubectl describe pod -n kube-system coredns-xxx

# RBAC 권한 확인
kubectl auth can-i list services --as=system:serviceaccount:kube-system:coredns
```

### 4.3 dig를 이용한 테스트

```bash
# 기본 A 레코드 조회
dig @localhost -p 53 example.com A

# DNSSEC 정보 포함
dig @localhost example.com A +dnssec

# TCP 강제
dig @localhost example.com A +tcp

# 간단한 응답
dig @localhost example.com A +short

# 역방향 조회
dig @localhost -x 10.0.0.1

# SRV 레코드 (K8s)
dig @localhost _http._tcp.my-svc.default.svc.cluster.local SRV

# Zone 전송
dig @localhost example.com AXFR

# EDNS0 버퍼 크기
dig @localhost example.com A +bufsize=4096
```

## 5. 성능 튜닝

### 5.1 캐시 최적화

```
cache 300 {
    success 20000 3600 60    # 양성: 용량 2만, 최대 TTL 1시간, 최소 TTL 1분
    denial 10000 600 10      # 음성: 용량 1만, 최대 TTL 10분, 최소 TTL 10초
    prefetch 10 1m 20%       # 10회 히트 + TTL 20% 이하 → 프리페치
    serve_stale 1h           # 만료 후 1시간까지 노화 서빙
}
```

### 5.2 Forward 최적화

```
forward . 8.8.8.8 8.8.4.4 1.1.1.1 {
    max_concurrent 2000          # 최대 동시 쿼리
    max_fails 3                  # 실패 임계값 증가
    expire 30s                   # 연결 캐시 수명 증가
    health_check 2s              # 헬스체크 간격 (부하 감소)
    policy round_robin           # 균등 분배
}
```

### 5.3 Kubernetes 최적화

```
kubernetes cluster.local in-addr.arpa ip6.arpa {
    ttl 60                       # TTL 증가 (기본 5초는 너무 짧을 수 있음)
    pods disabled                # Pod DNS 비활성화 (미사용 시)
}
```

### 5.4 시스템 레벨 튜닝

```bash
# GOMAXPROCS 자동 설정 (go.uber.org/automaxprocs)
# CoreDNS가 자동으로 처리 (coremain/run.go)

# 멀티소켓 (Linux SO_REUSEPORT)
.:53 {
    multisocket 4     # 4개 UDP 소켓으로 분산
    forward . 8.8.8.8
}

# 파일 디스크립터 제한 증가
ulimit -n 65535
```

### 5.5 버퍼 크기 조정

```
.:53 {
    bufsize 1232    # EDNS0 버퍼 크기 (DNS flag day 2020 권장)
    forward . 8.8.8.8
}
```

## 6. 보안 설정

### 6.1 TLS 설정

```
tls://.:853 {
    tls /etc/coredns/cert.pem /etc/coredns/key.pem /etc/coredns/ca.pem
    forward . 8.8.8.8
}
```

### 6.2 ACL (접근 제어)

```
.:53 {
    acl {
        allow net 10.0.0.0/8 192.168.0.0/16
        block net *
    }
    forward . 8.8.8.8
}
```

### 6.3 TSIG 인증

```
.:53 {
    tsig {
        secret mykey. base64-encoded-key
        require all
    }
    file db.example.com
    transfer {
        to * {
            tsig mykey.
        }
    }
}
```

### 6.4 DNSSEC 서명

```
.:53 {
    dnssec {
        key file Kexample.com
    }
    file db.example.com
}
```

## 7. 운영 체크리스트

### 배포 전

- [ ] Corefile 문법 검증 (`coredns -conf Corefile` 시작 확인)
- [ ] 업스트림 DNS 서버 연결 테스트
- [ ] 리소스 제한 설정 (CPU, 메모리)
- [ ] 헬스체크/레디니스 프로브 설정
- [ ] 메트릭 수집 설정 (Prometheus)

### 운영 중

- [ ] DNS 요청 처리량 모니터링
- [ ] 캐시 히트율 모니터링 (목표: 70% 이상)
- [ ] 응답 지연시간 모니터링 (p99 < 100ms)
- [ ] 업스트림 헬스체크 상태 확인
- [ ] 에러 로그 모니터링 (SERVFAIL, REFUSED)
- [ ] Corefile 변경 시 리로드 확인

### 장애 대응

- [ ] CoreDNS Pod 재시작 (K8s)
- [ ] 업스트림 DNS 변경 (forward 수정)
- [ ] 캐시 초기화 (Pod 재시작)
- [ ] 루프 감지 확인 (loop 플러그인)
- [ ] RBAC 권한 확인 (K8s 플러그인)
