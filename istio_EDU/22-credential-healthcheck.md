# 22. Credential 관리와 헬스체크 프록시 Deep-Dive

> Kubernetes Secret에서 TLS 인증서를 추출하여 SDS로 전달하는 Credential 관리와 pilot-agent가 대리 수행하는 애플리케이션 헬스체크 프록시

---

## 1. 개요

Istio의 데이터 플레인 보안과 가용성을 지탱하는 두 서브시스템:

1. **Credential 관리 (`pilot/pkg/credentials/`)**: Kubernetes Secret과 ConfigMap에서 TLS 인증서, CA 인증서, Docker 자격 증명을 추출하여 SDS(Secret Discovery Service)로 Envoy에 전달하는 메커니즘. 멀티클러스터 환경에서 여러 클러스터의 인증서를 통합 관리한다.

2. **헬스체크 프록시 (`pkg/istio-agent/health/`)**: Kubernetes의 Pod readinessProbe를 pilot-agent가 대리 수행하는 시스템. 사이드카가 트래픽을 인터셉트하므로 kubelet이 직접 애플리케이션 포트에 접근할 수 없는 문제를 해결한다.

```
┌──────────────────────────────────────────────────────────────┐
│                Credential 관리 아키텍처                         │
│                                                              │
│  Kubernetes Secret (TLS/Generic)                             │
│       │                                                      │
│       ▼                                                      │
│  CredentialsController                                       │
│  (kclient.Client[*v1.Secret])                               │
│       │                                                      │
│       ├── GetCertInfo(name, ns)  → CertInfo{Cert, Key, ...} │
│       ├── GetCaCert(name, ns)    → CertInfo{Cert, CRL}      │
│       └── Authorize(sa, ns)     → SubjectAccessReview       │
│       │                                                      │
│       ▼                                                      │
│  AggregateController (멀티클러스터)                             │
│       │                                                      │
│       ▼                                                      │
│  SDS Server → Envoy → mTLS/Gateway TLS                      │
└──────────────────────────────────────────────────────────────┘
```

---

## 2. Credential 관리 인터페이스

### 2.1 Controller 인터페이스

```
// pilot/pkg/credentials/model.go 라인 34-40

Controller 인터페이스:
  GetCertInfo(name, namespace string) (*CertInfo, error)
    → TLS 인증서 + 키 추출
  GetCaCert(name, namespace string) (*CertInfo, error)
    → CA 인증서 추출
  GetConfigMapCaCert(name, namespace string) (*CertInfo, error)
    → ConfigMap에서 CA 인증서 추출 (config 클러스터만)
  GetDockerCredential(name, namespace string) ([]byte, error)
    → Docker 레지스트리 자격 증명 추출
  Authorize(serviceAccount, namespace string) error
    → 서비스 계정의 Secret 읽기 권한 검증
```

### 2.2 MulticlusterController 인터페이스

```
// pilot/pkg/credentials/model.go 라인 42-45

MulticlusterController 인터페이스:
  ForCluster(cluster.ID) (Controller, error)
    → 특정 클러스터의 Controller 반환
  AddSecretHandler(func(kind.Kind, name, namespace string))
    → Secret 변경 이벤트 핸들러 등록
```

### 2.3 CertInfo 구조체

```
// pilot/pkg/credentials/model.go 라인 23-31

CertInfo:
  - Cert: []byte    // 인증서 체인 (PEM)
  - Key: []byte     // 개인 키 (PEM)
  - Staple: []byte  // OCSP 스테이플
  - CRL: []byte     // 인증서 폐지 목록
```

---

## 3. Kubernetes Secret에서 인증서 추출

### 3.1 지원하는 Secret 형식

```
// pilot/pkg/credentials/kube/secrets.go 라인 41-61

1. Kubernetes TLS Secret (kubernetes.io/tls):
   - tls.crt: 인증서 체인
   - tls.key: 개인 키
   - tls.ocsp-staple: OCSP 스테이플 (선택)
   - ca.crt: CA 인증서 (선택)
   - ca.crl: CRL (선택)

2. Generic Secret (Opaque):
   - cert: 인증서 체인
   - key: 개인 키
   - cacert: CA 인증서 (선택)
   - crl: CRL (선택)

3. Docker Config Secret (kubernetes.io/dockerconfigjson):
   - .dockerconfigjson: Docker 레지스트리 자격 증명
```

### 3.2 ExtractCertInfo 함수

```
// pilot/pkg/credentials/kube/secrets.go 라인 290-315

ExtractCertInfo(secret) 흐름:
  1. Generic Secret 키 확인 (cert, key):
     if hasValue(data, "cert", "key"):
       return CertInfo{Cert: data["cert"], Key: data["key"], CRL: data["crl"]}

  2. TLS Secret 키 확인 (tls.crt, tls.key):
     if hasValue(data, "tls.crt", "tls.key"):
       return CertInfo{
         Cert: data["tls.crt"],
         Key: data["tls.key"],
         Staple: data["tls.ocsp-staple"],
         CRL: data["ca.crl"],
       }

  3. 키가 있지만 값이 비어있는 경우:
     "found keys but they were empty" 에러

  4. 키 자체가 없는 경우:
     기대 키 목록 + 실제 키 목록 안내 에러
```

### 3.3 ExtractRoot (CA 인증서 추출)

```
// pilot/pkg/credentials/kube/secrets.go 라인 339-361

ExtractRoot(data):
  1. Generic 키 확인: cacert → CertInfo{Cert, CRL}
  2. TLS 키 확인: ca.crt → CertInfo{Cert, CRL}
  3. 키 있지만 비어있으면 에러
  4. 키 없으면 에러 + 가용 키 안내
```

---

## 4. CredentialsController 구현

### 4.1 구조체

```
// pilot/pkg/credentials/kube/secrets.go 라인 63-71

CredentialsController:
  - secrets: kclient.Client[*v1.Secret]           // Secret informer
  - configMaps: kclient.Client[*v1.ConfigMap]      // ConfigMap informer (config 클러스터만)
  - sar: SubjectAccessReviewInterface              // RBAC 검증 클라이언트
  - isConfigCluster: bool                          // config 클러스터 여부
  - mu: sync.RWMutex                               // 동시성 보호
  - authorizationCache: map[authorizationKey]authorizationResponse  // 인가 캐시
```

### 4.2 Secret 필터링

```
// pilot/pkg/credentials/kube/secrets.go 라인 90-92

SecretsFieldSelector:
  type != helm.sh/release.v1              // Helm 릴리스 시크릿 제외
  AND type != kubernetes.io/service-account-token  // SA 토큰 시크릿 제외

이유:
  - 대규모 클러스터에서 Helm 릴리스와 SA 토큰이 시크릿의 대부분
  - TLS 인증서와 Docker 자격 증명에만 관심
  - 최적화: 불필요한 캐시 메모리 사용 방지
```

### 4.3 인가 (Authorize)

```
// pilot/pkg/credentials/kube/secrets.go 라인 190-217

Authorize(serviceAccount, namespace) 흐름:
  1. 캐시 확인:
     user = sa.MakeUsername(namespace, serviceAccount)
     if cached = cachedAuthorization(user): return cached

  2. SubjectAccessReview 생성:
     SAR{
       Spec: {
         ResourceAttributes: { Namespace: ns, Verb: "list", Resource: "secrets" },
         User: "system:serviceaccount:ns:sa"
       }
     }

  3. API 서버에 SAR 전송
  4. 결과 캐싱 (성공: 5분, 실패: 1분)
  5. 허용/거부 반환

보안 의미:
  - 프록시가 자신의 네임스페이스 Secret만 읽을 수 있는지 검증
  - SDS 요청 시 호출자의 서비스 계정이 Secret 접근 권한 필요
```

### 4.4 인가 캐시

```
// pilot/pkg/credentials/kube/secrets.go 라인 147-188

authorizationCache:
  - TTL: 1분 (실패), 5분 (성공)
  - clearExpiredCache: 만료된 항목 정리
  - 키: authorizationKey(user)
  - 값: authorizationResponse{expiration, authorized error}

성공을 더 오래 캐시하는 이유:
  - Secret 접근 권한은 빈번히 변경되지 않음
  - 빠른 권한 취소가 필요하면 1분 후 자동 갱신
  - API 서버 부하 감소
```

---

## 5. 멀티클러스터 Credential 관리

### 5.1 Multicluster 구조체

```
// pilot/pkg/credentials/kube/multicluster.go 라인 27-31

Multicluster:
  - configCluster: cluster.ID                    // 설정 클러스터 ID
  - secretHandlers: []func(kind, name, ns)       // Secret 변경 핸들러
  - component: *multicluster.Component[*CredentialsController]  // 클러스터별 컨트롤러
```

### 5.2 ForCluster 로직

```
// pilot/pkg/credentials/kube/multicluster.go 라인 48-66

ForCluster(clusterID) 흐름:
  1. 요청 클러스터의 컨트롤러 조회
  2. AggregateController 생성:
     - authController: 요청 클러스터의 컨트롤러 (인가 전용)
     - controllers 목록:
       if clusterID != configCluster:
         [요청 클러스터, 설정 클러스터]  // 우선순위: 프록시 클러스터 > 설정 클러스터
       else:
         [설정 클러스터]

조회 우선순위:
  1. 프록시가 실행 중인 클러스터의 Secret 먼저 확인
  2. 설정 클러스터의 Secret에서 폴백
  3. 인가는 항상 프록시 클러스터에서 수행
```

### 5.3 AggregateController

```
// pilot/pkg/credentials/kube/multicluster.go 라인 73-153

AggregateController:
  - controllers: []*CredentialsController  // 순서대로 검색
  - authController: *CredentialsController // 인가 전담

GetCertInfo/GetCaCert/GetDockerCredential:
  for c in controllers:
    result, err = c.Get(name, namespace)
    if err == nil: return result
  return firstError  // 모든 클러스터에서 실패 시

Authorize:
  authController.Authorize(sa, ns)  // 프록시 클러스터에서만 검증
```

---

## 6. 헬스체크 프록시 아키텍처

### 6.1 문제 배경

```
사이드카 모드에서의 헬스체크 문제:

  kubelet ──→ Pod:8080/health (readinessProbe)
                  │
                  ▼
  iptables 규칙이 트래픽을 Envoy로 리다이렉트
                  │
                  ▼
  Envoy ──→ 앱:8080/health (mTLS 없이 접근 불가!)

해결: pilot-agent가 kubelet 대신 헬스체크를 수행하고 결과를 보고
```

### 6.2 Prober 인터페이스

```
// pkg/istio-agent/health/health_probers.go 라인 43-49

Prober 인터페이스:
  Probe(timeout time.Duration) (ProbeResult, error)

ProbeResult:
  Healthy   = "HEALTHY"
  Unhealthy = "UNHEALTHY"
  Unknown   = "UNKNOWN"

IsHealthy() bool: result == Healthy
```

### 6.3 Prober 구현체

#### HTTPProber

```
// pkg/istio-agent/health/health_probers.go 라인 122-216

HTTPProber:
  - Config: *v1alpha3.HTTPHealthCheckConfig
  - Transport: *http.Transport
  - DefaultHost: string

Probe 흐름:
  1. HTTP 클라이언트 생성 (timeout 설정)
  2. 커스텀 헤더 변환 (Config.HttpHeaders)
  3. URL 구성: scheme://host:port/path
  4. User-Agent: "istio-probe/1.0" (기본)
  5. GET 요청 전송
  6. 상태 코드 [200, 400) → Healthy
  7. 그 외 → Unhealthy

HTTPS 처리:
  - InsecureSkipVerify: true (localhost 헬스체크)
  - K8s kubelet과 동일한 동작 (TLS 검증 생략)
```

#### TCPProber

```
// pkg/istio-agent/health/health_probers.go 라인 218-251

TCPProber:
  - Config: *v1alpha3.TCPHealthCheckConfig
  - DefaultHost: string

Probe 흐름:
  1. status.ProbeDialer()로 다이얼러 생성
  2. TCP 연결 시도 (host:port)
  3. 연결 성공 → Healthy
  4. 연결 실패 → Unhealthy
  5. 연결 즉시 닫기
```

#### GRPCProber

```
// pkg/istio-agent/health/health_probers.go 라인 63-120

GRPCProber:
  - Config: *v1alpha3.GrpcHealthCheckConfig
  - DefaultHost: string

Probe 흐름:
  1. gRPC 연결 생성 (insecure, 타임아웃 포함)
  2. grpc_health_v1.HealthClient.Check 호출
  3. 서비스 이름: Config.Service
  4. SERVING → Healthy
  5. 기타 → Unhealthy

에러 코드 처리:
  - Unimplemented: gRPC 헬스 프로토콜 미지원
  - DeadlineExceeded: 타임아웃
  - 기타: 상태 코드와 함께 보고
```

#### ExecProber

```
// pkg/istio-agent/health/health_probers.go 라인 253-272

ExecProber:
  - Config: *v1alpha3.ExecHealthCheckConfig

Probe 흐름:
  1. exec.CommandContext로 명령 실행
  2. 타임아웃 컨텍스트 설정
  3. cmd.Run() 성공 → Healthy
  4. 타임아웃 → "command timeout exceeded"
  5. 기타 에러 → Unhealthy
```

#### EnvoyProber

```
// pkg/istio-agent/health/health_probers.go 라인 274-285

EnvoyProber:
  - Config: ready.Prober  // Envoy 준비 상태 확인기

Probe 흐름:
  1. Config.Check() 호출
  2. 에러 없음 → Healthy
  3. 에러 있음 → Unhealthy

역할: Envoy 프록시 자체의 준비 상태 확인
```

#### AggregateProber

```
// pkg/istio-agent/health/health_probers.go 라인 287-301

AggregateProber:
  - Probes: []Prober  // 여러 프로버 조합

Probe 흐름:
  for probe in Probes:
    result, err = probe.Probe(timeout)
    if !healthy: return result, err  // 하나라도 실패하면 중단
  return Healthy

조합 순서 (NewWorkloadHealthChecker):
  1. EnvoyProber (Envoy 준비 확인)
  2. 애플리케이션 프로버 (HTTP/TCP/gRPC/Exec)

의미: Envoy가 준비되지 않으면 앱 헬스체크 불필요
```

---

## 7. WorkloadHealthChecker

### 7.1 설정 기본값

```
// pkg/istio-agent/health/health_check.go 라인 54-75

fillInDefaults(cfg):
  FailureThreshold: max(cfg, 1)      // 최소 1
  SuccessThreshold: max(cfg, 1)      // 최소 1
  TimeoutSeconds: max(cfg, 1)        // 최소 1초
  PeriodSeconds: max(cfg, 10)        // 기본 10초

  HTTP 기본값:
    Path: "/" (비어있으면)
    Scheme: "http" (비어있으면, 소문자로 변환)
```

### 7.2 생성자

```
// pkg/istio-agent/health/health_check.go 라인 77-113

NewWorkloadHealthChecker(cfg, envoyProbe, proxyAddrs, ipv6):
  if cfg == nil: return nil  // 설정 없으면 no-op

  1. fillInDefaults 적용
  2. defaultHost 결정:
     - proxyAddrs가 있으면 첫 번째 주소 사용
     - 없으면 "localhost" (레거시 모드)
  3. 프로브 타입에 따른 Prober 생성:
     HttpGet → NewHTTPProber
     TcpSocket → NewTCPProber
     Exec → ExecProber
     Grpc → NewGRPCProber
  4. AggregateProber 구성:
     [EnvoyProber (있으면), 앱 Prober]
```

### 7.3 헬스체크 실행 루프

```
// pkg/istio-agent/health/health_check.go 라인 132-195

PerformApplicationHealthCheck(callback, quit):

  1. InitialDelay 대기 (앱 시작 시간)

  2. 상태 추적 변수:
     numSuccess, numFail = 0, 0
     lastState = lastStateUndefined

  3. 체크 루프:
     doCheck():
       result, err = prober.Probe(timeout)

       if Healthy:
         numSuccess++
         numFail = 0  // 연속 성공 카운트
         if numSuccess >= SuccessThreshold && lastState != Healthy:
           callback(ProbeEvent{Healthy: true})
           lastState = lastStateHealthy

       else:
         numFail++
         numSuccess = 0  // 연속 실패 카운트
         if numFail >= FailThreshold && lastState != Unhealthy:
           callback(ProbeEvent{
             Healthy: false,
             UnhealthyStatus: 500,
             UnhealthyMessage: err.Error(),
           })
           lastState = lastStateUnhealthy

  4. 첫 체크 즉시 실행
  5. PeriodSeconds 간격으로 반복
  6. quit 채널로 종료
```

### 7.4 상태 전이 다이어그램

```
                    ┌───────────────┐
                    │  Undefined    │
                    └───┬───────────┘
                        │ (첫 체크)
              ┌─────────┴─────────┐
              ▼                   ▼
     ┌────────────┐       ┌────────────┐
     │  Healthy   │       │ Unhealthy  │
     │ (callback) │       │ (callback) │
     └──────┬─────┘       └──────┬─────┘
            │ failThresh회 실패    │ successThresh회 성공
            ▼                    ▼
     ┌────────────┐       ┌────────────┐
     │ Unhealthy  │       │  Healthy   │
     │ (callback) │       │ (callback) │
     └────────────┘       └────────────┘

핵심: "상태 변경"시에만 callback 호출
  - Healthy → Healthy: callback 미호출 (불필요한 API 호출 방지)
  - Unhealthy → Unhealthy: callback 미호출
```

---

## 8. ProbeEvent와 xDS 통합

```
// pkg/istio-agent/health/health_check.go 라인 42-46

ProbeEvent:
  - Healthy: bool            // 건강 상태
  - UnhealthyStatus: int32   // 비정상 HTTP 상태 코드 (500)
  - UnhealthyMessage: string // 비정상 메시지

xDS 통합 흐름:
  1. pilot-agent가 ProbeEvent 수신
  2. 건강 상태 변경 시 WorkloadHealthHandler 호출
  3. EDS(Endpoint Discovery Service) 상태 업데이트
  4. 비정상이면 엔드포인트를 DRAINING으로 표시
  5. Envoy가 해당 엔드포인트로 트래픽 라우팅 중단
```

---

## 9. 설계 결정과 "왜(Why)"

### 9.1 왜 pilot-agent가 헬스체크를 대리하는가?

```
문제:
  1. 사이드카의 iptables 규칙이 모든 인바운드 트래픽을 Envoy로 리다이렉트
  2. kubelet의 헬스체크 요청도 Envoy를 통과
  3. mTLS가 활성화되면 kubelet은 mTLS 인증서가 없어 실패

해결:
  - pilot-agent가 "sidecar 내부"에서 직접 앱에 헬스체크
  - iptables 규칙에서 pilot-agent의 트래픽은 인터셉트 제외
  - status.ProbeDialer()로 특수 소스 IP 사용
  - 결과를 kubelet이 아닌 xDS로 보고

대안이 불가한 이유:
  - kubelet에 mTLS 인증서 제공: 불가능 (kubelet은 Istio 외부)
  - 헬스체크 포트를 인터셉트 제외: 보안 취약점 (평문 트래픽 허용)
```

### 9.2 왜 상태 변경 시에만 callback을 호출하는가?

```
연속 호출 방식:
  매 체크마다 callback → API 서버에 과도한 쓰기
  10초 간격 × 수천 파드 = 초당 수백 건의 업데이트

상태 변경 방식:
  Healthy→Unhealthy, Unhealthy→Healthy 시에만 callback
  정상 운영 중: callback 거의 미발생
  장애 시: 필요한 만큼만 업데이트

추가 최적화:
  연속 성공/실패 임계값으로 플래핑 방지:
  - FailureThreshold: 3 (연속 3회 실패해야 Unhealthy)
  - SuccessThreshold: 1 (1회 성공이면 Healthy)
```

### 9.3 왜 Secret 필드 선택자로 필터링하는가?

```
대규모 클러스터의 Secret 구성:
  - helm.sh/release.v1: Helm 릴리스 데이터 (대용량)
  - kubernetes.io/service-account-token: 모든 SA의 토큰
  - kubernetes.io/tls: TLS 인증서 (Istio 관심 대상)
  - Opaque: 범용 (일부가 Istio 관심 대상)

필드 선택자로 Helm + SA 토큰 제외:
  - 캐시 메모리 사용량 대폭 감소
  - watch 이벤트 수 감소 → API 서버 부하 감소
  - 코드 정확성에는 영향 없음 (최적화 전용)
```

### 9.4 왜 AggregateController에서 프록시 클러스터를 우선하는가?

```
시나리오: 프록시가 클러스터-B에서 실행, 설정 클러스터는 클러스터-A

검색 순서: [클러스터-B, 클러스터-A]

이유:
  1. 지역성: 같은 클러스터의 Secret이 더 가까움 (네트워크 지연)
  2. 보안: 프록시의 SA가 자신의 클러스터 Secret에 대한 권한이 더 확실
  3. 분리: 각 클러스터가 자체 인증서를 관리하되, 중앙 폴백 가능

인가: 항상 프록시 클러스터에서 수행
  - 프록시의 SA가 해당 클러스터에서 Secret 읽기 권한이 있어야 함
  - 원격 클러스터의 SA로 설정 클러스터의 Secret을 읽는 것은 위험
```

### 9.5 왜 EnvoyProber를 앱 프로버 앞에 배치하는가?

```
AggregateProber 순서:
  1. EnvoyProber  (Envoy 준비 확인)
  2. App Prober   (애플리케이션 헬스체크)

이유:
  - Envoy가 준비되지 않으면 트래픽을 받을 수 없음
  - 앱이 건강해도 Envoy가 준비되지 않으면 의미 없음
  - "빠른 실패": Envoy 미준비 시 즉시 Unhealthy 반환
  - 앱 프로버 실행 비용 절약
```

---

## 10. 소스 코드 경로 정리

| 파일 | 역할 |
|------|------|
| `pilot/pkg/credentials/model.go` | Controller/MulticlusterController/CertInfo 인터페이스 |
| `pilot/pkg/credentials/kube/secrets.go` | CredentialsController, Secret 추출, 인가, 캐시 |
| `pilot/pkg/credentials/kube/multicluster.go` | Multicluster, AggregateController |
| `pkg/istio-agent/health/health_probers.go` | Prober 인터페이스, HTTP/TCP/gRPC/Exec/Envoy/Aggregate 구현 |
| `pkg/istio-agent/health/health_check.go` | WorkloadHealthChecker, ProbeEvent, 헬스체크 루프 |

---

## 11. 운영 팁

### 11.1 인증서 Secret 디버깅

```
# Secret이 올바른 형식인지 확인
kubectl get secret my-tls -o jsonpath='{.type}'
# → kubernetes.io/tls 또는 Opaque

# 키 이름 확인
kubectl get secret my-tls -o jsonpath='{.data}' | jq 'keys'
# → ["tls.crt", "tls.key"] 또는 ["cert", "key"]

# istiod 로그에서 credential 조회 확인
kubectl logs -l app=istiod -c discovery | grep "credentials"
```

### 11.2 헬스체크 문제 진단

```
# pilot-agent 헬스체크 로그
kubectl logs <pod> -c istio-proxy | grep "healthcheck"

# Envoy 통계에서 헬스체크 결과 확인
kubectl exec <pod> -c istio-proxy -- pilot-agent request GET stats | grep health
```

---

## 12. 관련 PoC

- **poc-credential-controller**: TLS Secret에서 인증서를 추출하고 멀티클러스터 우선순위 기반으로 조회하는 시뮬레이션
- **poc-health-proxy**: HTTP/TCP/gRPC 헬스체크 프록시 시뮬레이션 (임계값 기반 상태 전이, AggregateProber)
