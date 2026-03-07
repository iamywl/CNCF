# Istio 시퀀스 다이어그램 (Sequence Diagrams)

이 문서는 Istio의 핵심 동작 흐름을 시퀀스 다이어그램으로 상세히 분석한다. 각 흐름은 실제 소스코드의 함수 호출과 데이터 흐름을 기반으로 작성되었으며, 설계 결정의 "왜(Why)"를 함께 설명한다.

---

## 목차

1. [xDS 설정 푸시 흐름](#1-xds-설정-푸시-흐름)
2. [mTLS 핸드셰이크 흐름](#2-mtls-핸드셰이크-흐름)
3. [사이드카 인젝션 흐름](#3-사이드카-인젝션-흐름)
4. [서비스 디스커버리 흐름](#4-서비스-디스커버리-흐름)
5. [인증서 로테이션 흐름](#5-인증서-로테이션-흐름)
6. [Ambient 메시 트래픽 흐름](#6-ambient-메시-트래픽-흐름)

---

## 1. xDS 설정 푸시 흐름

Istiod(Pilot)가 설정 변경을 감지하고 연결된 모든 Envoy 프록시에 xDS 업데이트를 푸시하는 전체 흐름이다. 디바운싱 메커니즘을 통해 대규모 설정 변경 시 과도한 푸시를 방지한다.

### 소스 파일 참조

| 파일 | 역할 |
|------|------|
| `pilot/pkg/xds/discovery.go` | DiscoveryServer, 디바운스, Push 로직 |
| `pilot/pkg/xds/ads.go` | ADS 스트림, pushConnection, PushOrder |
| `pilot/pkg/features/tuning.go` | 디바운스 기본값 정의 |

### 1.1 전체 흐름 개요

```
설정 변경 → ConfigUpdate() → pushChannel → debounce() → Push() → PushContext 재빌드
    → AdsPushAll() → StartPush() → PushQueue → doSendPushes() → pushConnection()
    → watchedResourcesByOrder() → CDS→EDS→LDS→RDS→SDS 순서로 gRPC 스트림 전송
```

### 1.2 Mermaid 시퀀스 다이어그램

```mermaid
sequenceDiagram
    participant Config as 설정 소스<br/>(K8s/MCP)
    participant DS as DiscoveryServer
    participant DC as debounce()
    participant PC as PushContext
    participant PQ as PushQueue
    participant SP as sendPushes()
    participant Conn as Connection
    participant Envoy as Envoy Proxy

    Note over Config,Envoy: 1단계: 설정 변경 감지 및 디바운싱

    Config->>DS: ConfigUpdate(PushRequest)
    Note right of DS: pushChannel <- req<br/>채널 버퍼 크기: 10<br/>(discovery.go:153)
    DS->>DC: pushChannel로 전송

    Note over DC: 디바운스 로직 시작<br/>DebounceAfter: 100ms<br/>debounceMax: 10s<br/>(features/tuning.go:77-88)

    loop 디바운스 대기
        Config->>DS: 추가 ConfigUpdate()
        DS->>DC: 이벤트 병합 (req.Merge)
        Note over DC: lastConfigUpdateTime 갱신<br/>debouncedEvents++
    end

    alt quietTime >= 100ms 또는 eventDelay >= 10s
        DC->>DS: pushFn(mergedRequest) 호출
    else EDS만 변경 && enableEDSDebounce=false
        DC->>DS: 즉시 pushFn(req) 호출<br/>(디바운스 우회)
    end

    Note over Config,Envoy: 2단계: PushContext 재빌드

    alt Full Push인 경우
        DS->>PC: initPushContext(req, oldPushContext, version)
        Note right of PC: NewPushContext()<br/>→ InitContext()<br/>→ SetPushContext()<br/>(discovery.go:520-530)
        PC-->>DS: 새 PushContext 반환
        DS->>DS: dropCacheForRequest(req)
        Note right of DS: Forced → ClearAll()<br/>아니면 → Clear(ConfigsUpdated)
    else Incremental Push
        DS->>DS: req.Push = globalPushContext()
        DS->>DS: dropCacheForRequest(req)
    end

    Note over Config,Envoy: 3단계: 모든 클라이언트에 푸시 분배

    DS->>DS: AdsPushAll(req)
    DS->>PQ: StartPush(req)

    loop 모든 연결된 클라이언트
        DS->>PQ: Enqueue(connection, req)
    end

    Note over Config,Envoy: 4단계: 큐에서 꺼내어 실제 푸시

    SP->>PQ: Dequeue() [블로킹]
    Note right of SP: concurrentPushLimit<br/>세마포어로 동시성 제한<br/>(features.PushThrottle)
    PQ-->>SP: (client, push)
    SP->>Conn: PushCh() <- pushEvent

    Note over Config,Envoy: 5단계: 연결별 푸시 실행

    Conn->>DS: pushConnection(conn, pushEv)

    alt Full Push
        DS->>DS: computeProxyState(proxy, pushRequest)
        Note right of DS: SetServiceTargets()<br/>SetWorkloadLabels()<br/>SetSidecarScope()<br/>(ads.go:385-446)
    end

    DS->>DS: ProxyNeedsPush(proxy, req)
    DS->>Conn: watchedResourcesByOrder()

    Note over Conn: PushOrder 정의 (ads.go:500-509):<br/>1. CDS (Cluster)<br/>2. EDS (Endpoint)<br/>3. LDS (Listener)<br/>4. RDS (Route)<br/>5. SDS (Secret)<br/>6. AddressType<br/>7. WorkloadType<br/>8. WorkloadAuthorizationType

    loop PushOrder 순서대로
        Conn->>Envoy: pushXds(con, watchedResource, req)
        Note right of Envoy: gRPC 스트림으로<br/>DiscoveryResponse 전송
    end
```

### 1.3 디바운스 메커니즘 상세

디바운스는 짧은 시간 내에 다수의 설정 변경이 발생할 때 이를 하나의 푸시로 합치는 핵심 최적화이다.

```
시간축 →

이벤트:  E1    E2  E3        E4
         │     │   │          │
         ▼     ▼   ▼          ▼
    ─────●─────●───●──────────●────────────────────────────
         │                    │
         │←─ DebounceAfter ──→│←─ DebounceAfter(100ms) ──→│
         │    (100ms)         │   조용한 시간 확보          │
         │                    │                            PUSH!
         │
         │←─────────── debounceMax (10s) ──────────────→│
                  최대 대기 시간 초과 시 강제 PUSH
```

**디바운스 상태 머신:**

```
                    ┌──────────────────────────┐
                    │                          │
                    ▼                          │
              ┌──────────┐    이벤트 수신     │
   이벤트 →   │   대기   │───────────────────┘
              │  (idle)  │    타이머 리셋
              └────┬─────┘
                   │
          timeChan │ 만료
                   │
              ┌────▼─────┐
              │ pushWorker│
              │  판단    │
              └────┬─────┘
                   │
          ┌────────┼────────┐
          │                 │
    quietTime >=       eventDelay >=
    DebounceAfter      debounceMax
          │                 │
          ▼                 ▼
    ┌───────────┐    ┌───────────┐
    │   PUSH    │    │ 강제 PUSH │
    │ (정상)    │    │ (타임아웃)│
    └───────────┘    └───────────┘
```

소스코드의 핵심 로직 (`discovery.go:364-388`):

```go
pushWorker := func() {
    eventDelay := time.Since(startDebounce)
    quietTime := time.Since(lastConfigUpdateTime)
    // 충분히 조용하거나 최대 대기 시간 초과
    if eventDelay >= opts.debounceMax || quietTime >= opts.DebounceAfter {
        if req != nil {
            free = false
            go push(req, debouncedEvents, startDebounce)
            req = nil
            debouncedEvents = 0
        }
    } else {
        timeChan = time.After(opts.DebounceAfter - quietTime)
    }
}
```

### 1.4 푸시 순서가 중요한 이유

`PushOrder`가 `CDS -> EDS -> LDS -> RDS -> SDS` 순서인 이유:

| 순서 | 타입 | 이유 |
|------|------|------|
| 1 | CDS (Cluster) | 업스트림 클러스터 정의가 먼저 있어야 함 |
| 2 | EDS (Endpoint) | 클러스터의 실제 엔드포인트 주소 |
| 3 | LDS (Listener) | 리스너가 클러스터를 참조 |
| 4 | RDS (Route) | 라우트가 리스너와 클러스터를 연결 |
| 5 | SDS (Secret) | TLS 인증서, 마지막에 적용해도 무방 |

이 순서를 지키지 않으면 Envoy가 아직 존재하지 않는 클러스터를 참조하는 리스너를 받게 되어 일시적으로 트래픽 라우팅이 실패할 수 있다.

### 1.5 동시성 제어

```
                    ┌─────────────────────────────┐
                    │  concurrentPushLimit         │
                    │  (semaphore channel)         │
                    │  크기: features.PushThrottle  │
                    └──────────┬──────────────────┘
                               │
               ┌───────────────┼───────────────┐
               │               │               │
          ┌────▼───┐     ┌────▼───┐     ┌────▼───┐
          │Push #1 │     │Push #2 │     │Push #3 │
          │(진행중)│     │(진행중)│     │(대기중)│
          └────┬───┘     └────┬───┘     └────────┘
               │               │
               ▼               ▼
          ┌────────┐     ┌────────┐
          │Envoy A │     │Envoy B │
          └────────┘     └────────┘
```

`doSendPushes()` 함수 (`discovery.go:469-513`)에서 세마포어 채널을 사용하여 동시 푸시 수를 제한한다. 푸시 완료 시 `doneFunc()`이 세마포어를 해제하고 `PushQueue.MarkDone()`을 호출한다.

---

## 2. mTLS 핸드셰이크 흐름

Istio 서비스 메시에서 두 워크로드 간 mTLS 통신이 성립되는 전체 흐름이다. iptables 투명 가로채기부터 SPIFFE 인증서 검증까지 포함한다.

### 소스 파일 참조

| 파일 | 역할 |
|------|------|
| `security/pkg/nodeagent/sds/sdsservice.go` | SDS 서비스, 인증서 스트리밍 |
| `security/pkg/nodeagent/cache/secretcache.go` | 인증서 캐시, CSR 생성 |
| `security/pkg/pki/ca/ca.go` | Istiod CA, 인증서 서명 |
| `security/pkg/nodeagent/caclient/providers/citadel/client.go` | CA 클라이언트 |

### 2.1 전체 통신 흐름

```mermaid
sequenceDiagram
    participant AppA as App A<br/>(소스)
    participant EnvoyA as Envoy A<br/>(사이드카)
    participant IPT as iptables<br/>(투명 프록시)
    participant EnvoyB as Envoy B<br/>(사이드카)
    participant AppB as App B<br/>(대상)

    Note over AppA,AppB: 사전 조건: 양쪽 Envoy가 SDS를 통해 SPIFFE 인증서를 보유

    AppA->>EnvoyA: HTTP 요청 (localhost:8080)
    Note right of AppA: 애플리케이션은 mTLS를<br/>인식하지 못함

    EnvoyA->>IPT: iptables OUTPUT 체인
    Note over IPT: REDIRECT 규칙으로<br/>아웃바운드 트래픽을<br/>포트 15001로 전환<br/>(istio-proxy UID 제외)

    IPT->>EnvoyA: 포트 15001<br/>(Envoy 아웃바운드 리스너)

    Note over EnvoyA: 1. 원본 목적지 IP 확인<br/>(SO_ORIGINAL_DST)<br/>2. 클러스터 매칭<br/>3. 로드밸런싱<br/>4. mTLS 업그레이드 결정

    EnvoyA->>EnvoyB: TLS ClientHello
    Note over EnvoyA,EnvoyB: mTLS 핸드셰이크 시작<br/>ALPN: istio-peer-exchange, h2

    EnvoyB->>EnvoyA: TLS ServerHello + Certificate
    Note right of EnvoyB: SPIFFE 인증서 제시<br/>SAN: spiffe://cluster.local/ns/{ns}/sa/{sa}

    EnvoyA->>EnvoyA: SPIFFE 인증서 검증
    Note right of EnvoyA: 1. 루트 CA 체인 검증<br/>2. SPIFFE URI SAN 확인<br/>3. AuthorizationPolicy 적용

    EnvoyA->>EnvoyB: TLS ClientCertificate
    Note left of EnvoyA: 자신의 SPIFFE 인증서 제시

    EnvoyB->>EnvoyB: 클라이언트 인증서 검증
    Note left of EnvoyB: 서버측 AuthorizationPolicy<br/>검증 수행

    EnvoyA->>EnvoyB: TLS Finished
    EnvoyB->>EnvoyA: TLS Finished

    Note over EnvoyA,EnvoyB: mTLS 터널 수립 완료

    EnvoyA->>EnvoyB: 암호화된 HTTP 요청 전송

    EnvoyB->>IPT: iptables PREROUTING 체인
    Note over IPT: 인바운드 트래픽을<br/>포트 15006으로 전환

    IPT->>EnvoyB: 포트 15006<br/>(Envoy 인바운드 리스너)

    EnvoyB->>AppB: 복호화된 HTTP 요청<br/>(localhost:{appPort})
    AppB->>EnvoyB: HTTP 응답
    EnvoyB->>EnvoyA: 암호화된 응답
    EnvoyA->>AppA: 복호화된 응답
```

### 2.2 인증서 발급 흐름

```mermaid
sequenceDiagram
    participant Envoy as Envoy Proxy
    participant SDS as SDS Service<br/>(sdsservice.go)
    participant SM as SecretManager<br/>(secretcache.go)
    participant CA as CA Client<br/>(citadel/client.go)
    participant Istiod as Istiod CA<br/>(ca/ca.go)

    Note over Envoy,Istiod: 워크로드 시작 시 인증서 발급

    Envoy->>SDS: StreamSecrets (gRPC 양방향 스트림)
    Note right of Envoy: 리소스 요청:<br/>- "default" (워크로드 cert)<br/>- "ROOTCA" (루트 cert)

    SDS->>SM: GenerateSecret("default")
    Note right of SM: 1. generateFileSecret() 시도<br/>2. getCachedSecret() 확인<br/>3. generateMutex 획득<br/>(secretcache.go:249-323)

    alt 캐시에 유효한 인증서 존재
        SM-->>SDS: 캐시된 SecretItem 반환
    else 새 인증서 필요
        SM->>SM: generateNewSecret()

        Note over SM: CSR 생성 과정<br/>(secretcache.go:778-856)

        SM->>SM: spiffe.Identity 구성
        Note right of SM: TrustDomain: cluster.local<br/>Namespace: {pod-ns}<br/>ServiceAccount: {pod-sa}

        SM->>SM: pkiutil.GenCSR(options)
        Note right of SM: RSA/ECC 키 생성<br/>+ CSR PEM 인코딩

        SM->>CA: CSRSign(csrPEM, ttl)
        CA->>Istiod: gRPC CreateCertificate

        Note over Istiod: CA 서명 과정 (ca.go:478-498):<br/>1. ParsePemEncodedCSR<br/>2. CheckSignature 검증<br/>3. TTL 확인 (maxCertTTL)<br/>4. SubjectIDs로 SAN 설정<br/>5. 서명 및 인증서 생성

        Istiod-->>CA: certChainPEM[] 반환
        CA-->>SM: certChainPEM + trustBundle 반환

        SM->>SM: registerSecret(item)
        Note right of SM: 캐시 저장 +<br/>로테이션 스케줄링<br/>(secretcache.go:877-901)
    end

    SM-->>SDS: SecretItem 반환
    SDS->>SDS: toEnvoySecret()
    Note right of SDS: SecretItem →<br/>Envoy tls.Secret 변환<br/>(sdsservice.go:292-398)
    SDS-->>Envoy: DiscoveryResponse<br/>(SDS 타입)
```

### 2.3 iptables 규칙 구조

```
                        ┌─────────────────────────────────────────┐
                        │              Pod Network Namespace       │
                        │                                         │
   Inbound Traffic      │    ┌──────────────────────────┐         │
  ────────────────────► │    │  PREROUTING (nat)         │         │
                        │    │                           │         │
                        │    │  ISTIO_INBOUND 체인       │         │
                        │    │  ┌───────────────────┐   │         │
                        │    │  │ 포트 15008 제외    │   │         │
                        │    │  │ 포트 15090 제외    │   │         │
                        │    │  │ 포트 15021 제외    │   │         │
                        │    │  │ 나머지 → 15006    │   │         │
                        │    │  └───────────────────┘   │         │
                        │    └──────────────────────────┘         │
                        │                                         │
                        │    ┌─────────┐     ┌──────────┐         │
                        │    │ Envoy   │     │  App     │         │
                        │    │ 15006   │────►│ :{port}  │         │
                        │    │ (인바운드)│     │          │         │
                        │    │         │     │          │         │
                        │    │ 15001   │◄────│          │         │
                        │    │(아웃바운드)│     │          │         │
                        │    └────┬────┘     └──────────┘         │
                        │         │                               │
                        │    ┌────▼─────────────────────┐         │
                        │    │  OUTPUT (nat)             │         │
                        │    │                           │         │
                        │    │  ISTIO_OUTPUT 체인        │         │
                        │    │  ┌───────────────────┐   │         │
                        │    │  │ istio-proxy UID    │   │         │
                        │    │  │ 에서 보낸 트래픽   │   │         │
                        │    │  │ → RETURN (우회)    │   │         │
                        │    │  │                   │   │         │
                        │    │  │ 나머지 앱 트래픽   │   │         │
                        │    │  │ → REDIRECT 15001  │   │         │
                        │    │  └───────────────────┘   │         │
                        │    └──────────────────────────┘         │
   Outbound Traffic     │                                         │
  ◄──────────────────── │                                         │
                        └─────────────────────────────────────────┘
```

### 2.4 SPIFFE 인증서 구조

```
X.509 Certificate:
├── Subject: O=<trust-domain>
├── Subject Alternative Name (SAN):
│   └── URI: spiffe://cluster.local/ns/default/sa/my-service
├── Key Usage: Digital Signature, Key Encipherment
├── Extended Key Usage: Server Auth, Client Auth
├── Issuer: Istiod CA (self-signed 또는 plugged-in)
└── Validity:
    ├── Not Before: <발급시간>
    └── Not After: <발급시간 + SecretTTL>
```

---

## 3. 사이드카 인젝션 흐름

Kubernetes의 Mutating Admission Webhook을 이용해 Pod 생성 시 자동으로 Envoy 사이드카 컨테이너를 주입하는 흐름이다.

### 소스 파일 참조

| 파일 | 역할 |
|------|------|
| `pkg/kube/inject/webhook.go` | Webhook 서버, inject(), injectPod(), createPatch() |
| `pkg/kube/inject/inject.go` | injectRequired(), RunTemplate() |

### 3.1 전체 인젝션 흐름

```mermaid
sequenceDiagram
    participant User as 사용자/CD
    participant API as K8s API Server
    participant WH as Istiod Webhook<br/>(webhook.go)
    participant INJ as Injection Engine<br/>(inject.go)

    User->>API: kubectl apply -f deployment.yaml
    Note right of User: Pod 생성 요청

    API->>API: Admission 단계 진입
    Note right of API: MutatingWebhookConfiguration에<br/>등록된 웹훅 호출

    API->>WH: POST /inject<br/>(AdmissionReview)
    Note right of WH: Content-Type: application/json<br/>serveInject() 진입<br/>(webhook.go:1288)

    WH->>WH: 요청 검증
    Note right of WH: 1. Body 존재 확인<br/>2. Content-Type 확인<br/>3. AdmissionReview 디코딩<br/>(webhook.go:1293-1336)

    WH->>WH: inject(ar, path)
    Note right of WH: (webhook.go:1104)

    WH->>WH: JSON Unmarshal → Pod
    Note right of WH: req.Object.Raw → corev1.Pod<br/>ManagedFields 제거 (CPU 절약)

    WH->>INJ: injectRequired() 호출
    Note right of INJ: (inject.go:199)<br/>정책 판단 로직

    Note over INJ: injectRequired 판단 기준:<br/>1. IgnoredNamespaces 확인<br/>   (kube-system, kube-public 등)<br/>2. host network 모드 확인<br/>3. 이미 주입된 Pod 확인<br/>4. Pod 어노테이션 확인<br/>   (sidecar.istio.io/inject)<br/>5. 네임스페이스 라벨 확인<br/>   (istio-injection=enabled)<br/>6. NeverInjectSelector 매칭<br/>7. AlwaysInjectSelector 매칭<br/>8. Config.Policy 확인<br/>   (enabled/disabled)

    alt 인젝션 불필요
        INJ-->>WH: false 반환
        WH-->>API: AdmissionResponse<br/>{Allowed: true, Patch: nil}
        Note right of WH: 인젝션 스킵<br/>totalSkippedInjections++
    else 인젝션 필요
        INJ-->>WH: true 반환

        WH->>WH: InjectionParameters 구성
        Note right of WH: (webhook.go:1141-1154)<br/>- pod, deployMeta, typeMeta<br/>- templates, meshConfig<br/>- proxyConfig, valuesConfig<br/>- revision, proxyEnvs

        WH->>WH: injectPod(params)
        Note right of WH: (webhook.go:463)

        WH->>WH: originalPodSpec = json.Marshal(pod)
        Note right of WH: 원본 Pod 스냅샷 저장

        WH->>INJ: RunTemplate(params)
        Note right of INJ: (inject.go:430)<br/>Go 텍스트 템플릿 실행

        Note over INJ: RunTemplate 과정:<br/>1. 어노테이션 검증<br/>2. 템플릿 선택<br/>   (기본 또는 커스텀)<br/>3. 템플릿 파라미터 바인딩<br/>   - ProxyConfig<br/>   - MeshConfig<br/>   - Values<br/>4. 템플릿 렌더링<br/>   → istio-init 컨테이너<br/>   → istio-proxy 컨테이너<br/>   → 볼륨 마운트<br/>5. 원본 Pod와 병합

        INJ-->>WH: (mergedPod, injectedPodData)

        WH->>WH: reapplyOverwrittenContainers()
        Note right of WH: 사용자 정의 컨테이너<br/>설정 재적용

        WH->>WH: postProcessPod()
        Note right of WH: DNS, 프로메테우스<br/>어노테이션 처리

        WH->>WH: createPatch(mergedPod, originalPodSpec)
        Note right of WH: (webhook.go:726)<br/>JSON Patch 생성<br/>jsonpatch.CreatePatch()

        WH-->>API: AdmissionResponse<br/>{Allowed: true,<br/> Patch: patchBytes,<br/> PatchType: "JSONPatch"}
    end

    API->>API: JSON Patch 적용
    Note right of API: 원본 Pod에 패치 적용<br/>→ 사이드카 포함 Pod 확정

    API-->>User: Pod 생성 완료
```

### 3.2 인젝션 결정 흐름도

```
                      Pod 생성 요청
                           │
                           ▼
                ┌─────────────────────┐
                │ IgnoredNamespaces?  │──── Yes ──→ SKIP
                │ (kube-system 등)    │
                └──────────┬──────────┘
                           │ No
                           ▼
                ┌─────────────────────┐
                │ Host Network 모드?  │──── Yes ──→ SKIP
                └──────────┬──────────┘
                           │ No
                           ▼
                ┌─────────────────────┐
                │ 이미 사이드카 존재? │──── Yes ──→ SKIP
                │ (istio-proxy 확인)  │
                └──────────┬──────────┘
                           │ No
                           ▼
                ┌─────────────────────┐
                │ Pod 어노테이션      │
                │ sidecar.istio.io/   │
                │ inject = "false"?   │──── Yes ──→ SKIP
                └──────────┬──────────┘
                           │ No/"true"/없음
                           ▼
                ┌─────────────────────┐
                │ NeverInjectSelector │──── 매칭 ──→ SKIP
                │ 라벨 매칭?          │
                └──────────┬──────────┘
                           │ 불일치
                           ▼
                ┌─────────────────────┐
                │ AlwaysInjectSelector│──── 매칭 ──→ INJECT
                │ 라벨 매칭?          │
                └──────────┬──────────┘
                           │ 불일치
                           ▼
                ┌─────────────────────┐
                │ Namespace 라벨      │
                │ istio-injection=    │
                │ enabled?            │──── Yes ──→ INJECT
                │ 또는 istio.io/rev   │
                │ 매칭?               │
                └──────────┬──────────┘
                           │ No
                           ▼
                ┌─────────────────────┐
                │ Config.Policy =     │──── Yes ──→ INJECT
                │ enabled?            │
                └──────────┬──────────┘
                           │ No
                           ▼
                         SKIP
```

### 3.3 주입되는 컨테이너 구조

```
Pod (인젝션 후)
├── initContainers:
│   └── istio-init                          ← iptables 규칙 설정
│       ├── image: proxyv2
│       ├── command: ["istio-iptables"]
│       └── securityContext:
│           └── capabilities: [NET_ADMIN, NET_RAW]
│
├── containers:
│   ├── {원본 앱 컨테이너}
│   └── istio-proxy                         ← Envoy 사이드카
│       ├── image: proxyv2
│       ├── ports: [15090(prometheus), 15021(healthz)]
│       ├── env: [ISTIO_META_*, POD_NAME, POD_NAMESPACE, ...]
│       ├── volumeMounts:
│       │   ├── istio-envoy (emptyDir)      ← Envoy 설정 + UDS
│       │   ├── istio-data (emptyDir)       ← 런타임 데이터
│       │   ├── istio-token (projected)     ← ServiceAccount 토큰
│       │   └── istiod-ca-cert (configMap)  ← CA 루트 인증서
│       └── readinessProbe: /healthz/ready:15021
│
└── volumes:
    ├── istio-envoy (emptyDir, medium: Memory)
    ├── istio-data (emptyDir)
    ├── istio-token (projected serviceAccountToken)
    └── istiod-ca-cert (configMap: istio-ca-root-cert)
```

### 3.4 JSON Patch 예시

`createPatch()` 함수 (`webhook.go:726-743`)가 생성하는 패치 형식:

```json
[
  {
    "op": "add",
    "path": "/spec/initContainers",
    "value": [{"name": "istio-init", ...}]
  },
  {
    "op": "add",
    "path": "/spec/containers/-",
    "value": {"name": "istio-proxy", ...}
  },
  {
    "op": "add",
    "path": "/spec/volumes/-",
    "value": {"name": "istio-envoy", ...}
  },
  {
    "op": "add",
    "path": "/metadata/labels/security.istio.io~1tlsMode",
    "value": "istio"
  },
  {
    "op": "add",
    "path": "/metadata/annotations/sidecar.istio.io~1status",
    "value": "{\"initContainers\":[\"istio-init\"],\"containers\":[\"istio-proxy\"],...}"
  }
]
```

---

## 4. 서비스 디스커버리 흐름

Kubernetes 서비스/엔드포인트 변경이 Informer를 통해 감지되고, Istio 내부 모델로 변환되어 xDS 푸시로 이어지는 흐름이다.

### 소스 파일 참조

| 파일 | 역할 |
|------|------|
| `pilot/pkg/serviceregistry/kube/controller/controller.go` | K8s 서비스 레지스트리 컨트롤러 |
| `pilot/pkg/serviceregistry/kube/conversion.go` | K8s 서비스 → Istio 모델 변환 |
| `pilot/pkg/xds/discovery.go` | ConfigUpdate 수신, xDS 푸시 트리거 |

### 4.1 서비스 이벤트 처리 흐름

```mermaid
sequenceDiagram
    participant K8s as K8s API Server
    participant Inf as Informer<br/>(kclient)
    participant Ctrl as Controller<br/>(controller.go)
    participant Map as servicesMap
    participant XDS as XDSUpdater
    participant DS as DiscoveryServer

    Note over K8s,DS: 1단계: K8s 이벤트 수신

    K8s->>Inf: Service 변경 이벤트
    Note right of Inf: kclient.NewFiltered[*v1.Service]<br/>ObjectFilter로 필터링

    Inf->>Ctrl: onServiceEvent(pre, curr, event)
    Note right of Ctrl: (controller.go:433)

    Note over Ctrl: 2단계: 모델 변환

    Ctrl->>Ctrl: kube.ConvertService(*curr, ...)
    Note right of Ctrl: K8s Service → model.Service<br/>- Hostname 생성<br/>  (svc.ns.svc.cluster.local)<br/>- ClusterIP, Ports 변환<br/>- ExportTo 설정<br/>- Traffic Distribution<br/>  네임스페이스 상속

    Note over Ctrl: 3단계: 내부 상태 업데이트

    alt Delete 이벤트
        Ctrl->>Map: delete(servicesMap, hostname)
        Ctrl->>XDS: SvcUpdate(shard, hostname, ns, EventDelete)
        Ctrl->>Ctrl: handlers.NotifyServiceHandlers(nil, svc, EventDelete)
    else Add/Update 이벤트
        Ctrl->>Ctrl: addOrUpdateService(pre, curr, svcConv, event)
        Note right of Ctrl: (controller.go:527)

        Ctrl->>Ctrl: extractGatewaysFromService()
        Note right of Ctrl: 네트워크 게이트웨이 확인

        Ctrl->>Map: servicesMap[hostname] = svcConv

        alt 네트워크 게이트웨이 변경
            Ctrl->>XDS: ConfigUpdate(Full, NetworksTrigger)
            Note right of XDS: 모든 EDS 엔드포인트<br/>업데이트 필요
        end

        Ctrl->>XDS: EDSCacheUpdate(shard, hostname, ns, endpoints)
        Note right of XDS: 엔드포인트 캐시 업데이트

        Ctrl->>XDS: SvcUpdate(shard, hostname, ns, event)
    end

    Note over Ctrl: 4단계: 서비스 핸들러 알림

    Ctrl->>Ctrl: handlers.NotifyServiceHandlers(prev, curr, event)
    Note right of Ctrl: 등록된 핸들러들에게<br/>서비스 변경 통보

    Note over XDS,DS: 5단계: xDS 푸시 트리거

    XDS->>DS: ConfigUpdate(PushRequest)
    Note right of DS: pushChannel <- req<br/>디바운스 → Push → xDS 스트림
```

### 4.2 엔드포인트 변경 흐름

```mermaid
sequenceDiagram
    participant K8s as K8s API Server
    participant ES as EndpointSlice<br/>Controller
    participant PC as PodCache
    participant Ctrl as Controller
    participant XDS as XDSUpdater

    K8s->>ES: EndpointSlice 변경 이벤트

    ES->>ES: onEndpointSliceEvent()
    Note right of ES: EndpointSlice → 내부 모델 변환

    ES->>PC: Pod IP → Pod 매핑 조회

    alt Pod가 아직 준비되지 않음
        PC->>PC: endpointsPendingPodUpdate++
        Note right of PC: Pod 도착 대기<br/>podArrived() 콜백 등록
    else Pod 정보 확인됨
        ES->>ES: 엔드포인트 빌드
        Note right of ES: IP, Port, Labels,<br/>Locality, Network 설정

        ES->>XDS: EDSUpdate(shard, hostname, ns, endpoints)
        Note right of XDS: Incremental Push 트리거<br/>(Full이 아닌 EDS만)
    end
```

### 4.3 서비스 레지스트리 아키텍처

```
                     ┌─────────────────────────────┐
                     │     Aggregate Controller     │
                     │   (여러 레지스트리 통합)       │
                     └──────────┬──────────────────┘
                                │
              ┌─────────────────┼─────────────────┐
              │                 │                  │
     ┌────────▼──────┐  ┌──────▼──────┐   ┌──────▼──────┐
     │ K8s Controller │  │ K8s Controller│  │ServiceEntry │
     │ (Cluster A)   │  │ (Cluster B)  │  │ Controller  │
     └───────┬───────┘  └──────┬───────┘  └──────┬──────┘
             │                  │                  │
     ┌───────▼───────┐  ┌──────▼───────┐         │
     │  Informers    │  │  Informers   │         │
     │ ┌───────────┐ │  │ ┌───────────┐│         │
     │ │ Services  │ │  │ │ Services  ││         │
     │ │ Endpoints │ │  │ │ Endpoints ││         │
     │ │ Pods      │ │  │ │ Pods      ││         │
     │ │ Nodes     │ │  │ │ Nodes     ││         │
     │ └───────────┘ │  │ └───────────┘│         │
     └───────┬───────┘  └──────┬───────┘         │
             │                  │                  │
             ▼                  ▼                  ▼
     ┌─────────────────────────────────────────────┐
     │              servicesMap                     │
     │  map[host.Name]*model.Service               │
     │                                             │
     │  "svc-a.ns.svc.cluster.local" → Service{}   │
     │  "svc-b.ns.svc.cluster.local" → Service{}   │
     └─────────────────────────────────────────────┘
             │
             ▼
     ┌──────────────────┐
     │  XDSUpdater      │
     │  → ConfigUpdate  │
     │  → EDSUpdate     │
     │  → SvcUpdate     │
     └──────────────────┘
```

### 4.4 서비스 변환 테이블

K8s Service가 Istio 내부 모델로 변환되는 필드 매핑:

| K8s Service 필드 | Istio model.Service 필드 | 설명 |
|------------------|--------------------------|------|
| `metadata.name` + `metadata.namespace` | `Hostname` | `{name}.{ns}.svc.{domain}` 형식 |
| `spec.clusterIP` | `DefaultAddress` | 클러스터 IP |
| `spec.ports[]` | `Ports[]` | 포트, 프로토콜 변환 |
| `spec.type=LoadBalancer` | `ClusterExternalAddresses` | 외부 IP/호스트네임 |
| `spec.externalIPs` | `ClusterExternalAddresses` | 외부 IP 목록 |
| `metadata.labels` | `Attributes.Labels` | 서비스 라벨 |
| `metadata.annotations[networking.istio.io/exportTo]` | `Attributes.ExportTo` | 가시성 범위 |

---

## 5. 인증서 로테이션 흐름

워크로드 인증서의 만료 전 자동 갱신 메커니즘이다. Grace Period 비율과 Jitter를 활용하여 대규모 플릿에서 동시 갱신으로 인한 부하 집중을 방지한다.

### 소스 파일 참조

| 파일 | 역할 |
|------|------|
| `security/pkg/nodeagent/cache/secretcache.go` | 로테이션 타이밍, 캐시 관리 |
| `security/pkg/nodeagent/sds/sdsservice.go` | SDS 푸시 |
| `pilot/cmd/pilot-agent/options/options.go` | 로테이션 파라미터 기본값 |

### 5.1 인증서 로테이션 전체 흐름

```mermaid
sequenceDiagram
    participant Q as Delayed Queue
    participant SM as SecretManager<br/>(secretcache.go)
    participant SDS as SDS Service
    participant CA as CA Client
    participant Istiod as Istiod CA
    participant Envoy as Envoy Proxy

    Note over Q,Envoy: 사전 조건: 초기 인증서가 발급되어 registerSecret() 완료

    Note over SM: registerSecret() 시점에<br/>로테이션 타이머 등록<br/>(secretcache.go:877-901)

    SM->>SM: rotateTime() 계산
    Note right of SM: graceRatio = 0.5 (기본값)<br/>jitter = +/- 0.01 (기본값)<br/><br/>secretLifeTime = ExpireTime - CreatedTime<br/>jitterGraceRatio = graceRatio + random*jitter<br/>gracePeriod = jitterGraceRatio * secretLifeTime<br/>delay = ExpireTime - gracePeriod - Now

    SM->>Q: queue.PushDelayed(rotateFunc, delay)
    Note right of Q: 예: TTL=24h일 때<br/>delay ~= 12h (50% 시점)

    Note over Q,Envoy: ... delay 시간 경과 ...

    Q->>SM: rotateFunc() 실행

    SM->>SM: 캐시 유효성 확인
    Note right of SM: cached.CreatedTime == item.CreatedTime<br/>→ stale 스케줄 아닌지 확인

    SM->>SM: cache.SetWorkload(nil)
    Note right of SM: 캐시 초기화<br/>→ 다음 GenerateSecret()에서<br/>새 인증서 발급 유도

    SM->>SDS: OnSecretUpdate("default")
    Note right of SDS: secretHandler 콜백 호출

    SDS->>SDS: push("default")
    Note right of SDS: (sdsservice.go:162-173)<br/>모든 SDS 클라이언트에게<br/>업데이트 알림

    loop 각 SDS 클라이언트
        SDS->>Envoy: PushCh() <- "default"

        Note over Envoy: Envoy가 새 인증서 요청

        Envoy->>SDS: SDS 스트림에서 재요청
        SDS->>SM: GenerateSecret("default")

        SM->>SM: getCachedSecret() → nil
        Note right of SM: 캐시가 비어있으므로<br/>새 인증서 생성 필요

        SM->>SM: generateNewSecret()
        SM->>CA: CSRSign(newCSR, ttl)
        CA->>Istiod: CreateCertificate(CSR)
        Istiod-->>CA: 새 인증서 체인
        CA-->>SM: certChainPEM

        SM->>SM: registerSecret(newItem)
        Note right of SM: 새 캐시 저장 +<br/>다음 로테이션 스케줄링

        SM-->>SDS: 새 SecretItem
        SDS-->>Envoy: 새 인증서 전달<br/>(DiscoveryResponse)
    end
```

### 5.2 로테이션 타이밍 계산

`rotateTime()` 함수의 동작 (`secretcache.go:858-875`):

```
인증서 타임라인 (TTL = 24시간 기준, graceRatio = 0.5):

발급시간                                              만료시간
  │                                                      │
  ▼                                                      ▼
  ├──────────────────────────────────────────────────────┤
  │                    24시간 (TTL)                       │
  │                                                      │
  │              ┌──── Grace Period (12h) ───────────────┤
  │              │    (= TTL * graceRatio)               │
  │              │                                       │
  │◄─ delay ───►│                                       │
  │  (~12시간)   │                                       │
  │              │                                       │
  │         로테이션                                    만료
  │          시점                                       시점

  * jitter (+-0.01)로 실제 로테이션 시점은 약간씩 다름
  * 이를 통해 대규모 플릿에서 동시 갱신 방지
```

**Jitter 분포 예시 (1000개 워크로드):**

```
로테이션 시점 분포 (graceRatio=0.5, jitter=0.01):

빈도
  │
  │          ████
  │         ██████
  │        ████████
  │       ██████████
  │      ████████████
  │     ██████████████
  │    ████████████████
  │   ██████████████████
  │  ████████████████████
  └──┼────┼────┼────┼────┼── 시간
     49%  49.5% 50%  50.5% 51%
              TTL 경과 비율
```

### 5.3 로테이션 상태 머신

```
                    ┌───────────────────┐
                    │   VALID           │
                    │ (인증서 유효)      │
                    └────────┬──────────┘
                             │
                    delay 만료 (PushDelayed 트리거)
                             │
                    ┌────────▼──────────┐
                    │  ROTATING         │
                    │ cache.SetWorkload │
                    │     (nil)         │
                    └────────┬──────────┘
                             │
                    OnSecretUpdate("default")
                             │
                    ┌────────▼──────────┐
                    │  CSR_PENDING      │
                    │ generateNewSecret │
                    │ → CSRSign()       │
                    └────────┬──────────┘
                             │
                    ┌────────┼────────┐
                    │                 │
               성공 │            실패 │
                    │                 │
           ┌───────▼───────┐  ┌──────▼───────┐
           │ RENEWED        │  │ RETRY        │
           │ registerSecret │  │ backoff 후   │
           │ → 새 캐시 저장 │  │ 재시도       │
           │ → 새 타이머    │  └──────────────┘
           └───────┬───────┘
                   │
                   ▼
           ┌───────────────┐
           │   VALID       │ (순환 반복)
           └───────────────┘
```

### 5.4 파일 기반 인증서 감시

파일 마운트된 인증서의 경우 로테이션 대신 파일 변경 감시를 사용한다:

```mermaid
sequenceDiagram
    participant FS as File System
    participant FW as fsnotify Watcher<br/>(certWatcher)
    participant SM as SecretManager
    participant SDS as SDS Service
    participant Envoy as Envoy Proxy

    Note over FS,Envoy: 외부 시스템이 인증서 파일 갱신

    FS->>FW: WRITE/CREATE/REMOVE 이벤트

    FW->>SM: handleFileWatch()
    Note right of SM: (secretcache.go:903-937)

    SM->>SM: handleFileEvent()
    Note right of SM: 이벤트 파일명과<br/>fileCerts 맵 대조

    alt 심볼릭 링크 인증서
        SM->>SM: handleSymlinkChange(fc)
        Note right of SM: 심볼릭 링크 재해석<br/>새 타겟 경로 등록
    end

    SM->>SM: OnSecretUpdate(resourceName)
    SM->>SDS: secretHandler(resourceName)
    SDS->>SDS: push(resourceName)
    SDS->>Envoy: 새 인증서 스트림 전송
```

---

## 6. Ambient 메시 트래픽 흐름

Ambient 모드는 사이드카 없이 ztunnel(L4 프록시)과 선택적 waypoint 프록시(L7)를 사용하여 메시 기능을 제공한다.

### 소스 파일 참조

| 파일 | 역할 |
|------|------|
| `pilot/pkg/serviceregistry/kube/controller/controller.go` | Ambient 인덱스 초기화 |
| `pilot/pkg/serviceregistry/kube/controller/ambient/` | Ambient 워크로드/정책 인덱스 |
| `pilot/pkg/xds/ads.go` | WorkloadType, WorkloadAuthorizationType |

### 6.1 사이드카 모드 vs Ambient 모드 비교

```
사이드카 모드:                    Ambient 모드:

┌────────────────┐              ┌────────────────┐
│     Pod A      │              │     Pod A      │
│ ┌────────────┐ │              │ ┌────────────┐ │
│ │   App      │ │              │ │   App      │ │
│ └─────┬──────┘ │              │ └─────┬──────┘ │
│       │        │              │       │        │
│ ┌─────▼──────┐ │              └───────┼────────┘
│ │ Envoy      │ │                      │
│ │ (sidecar)  │ │              ┌───────▼────────┐
│ └─────┬──────┘ │              │   ztunnel      │
└───────┼────────┘              │  (노드 레벨    │
        │                       │   L4 프록시)   │
     mTLS                       └───────┬────────┘
        │                               │
┌───────▼────────┐              ┌───────▼────────┐
│     Pod B      │              │  HBONE 터널    │
│ ┌────────────┐ │              │  (mTLS over    │
│ │ Envoy      │ │              │   HTTP/2)      │
│ │ (sidecar)  │ │              │  포트: 15008   │
│ └─────┬──────┘ │              └───────┬────────┘
│       │        │                      │
│ ┌─────▼──────┐ │              ┌───────▼────────┐
│ │   App      │ │              │   ztunnel      │
│ └────────────┘ │              │  (대상 노드)    │
└────────────────┘              └───────┬────────┘
                                        │
                                ┌───────▼────────┐
                                │     Pod B      │
                                │ ┌────────────┐ │
                                │ │   App      │ │
                                │ └────────────┘ │
                                └────────────────┘
```

### 6.2 Ambient L4 전용 흐름 (ztunnel-to-ztunnel)

```mermaid
sequenceDiagram
    participant AppA as App A<br/>(소스 Pod)
    participant ZtA as ztunnel A<br/>(소스 노드)
    participant ZtB as ztunnel B<br/>(대상 노드)
    participant AppB as App B<br/>(대상 Pod)

    Note over AppA,AppB: L4 전용 (waypoint 없음) - mTLS 암호화 + 전달

    AppA->>ZtA: TCP 연결<br/>(앱은 평문 전송)
    Note right of AppA: ztunnel이 투명하게<br/>트래픽 가로채기<br/>(tproxy/eBPF)

    ZtA->>ZtA: 대상 서비스 확인
    Note right of ZtA: 1. 대상 IP/포트 매핑<br/>2. AuthorizationPolicy 확인<br/>3. L4 정책 적용

    ZtA->>ZtB: HBONE 터널 수립<br/>(포트 15008)
    Note over ZtA,ZtB: HBONE = HTTP/2 CONNECT<br/>over mTLS<br/><br/>mTLS with SPIFFE certs:<br/>- 클라이언트: 소스 워크로드 ID<br/>- 서버: 대상 워크로드 ID

    ZtA->>ZtB: HTTP/2 CONNECT 요청
    Note right of ZtB: CONNECT 메서드로<br/>터널 대상 지정

    ZtB->>ZtB: 인바운드 정책 확인
    Note right of ZtB: AuthorizationPolicy<br/>L4 규칙 적용

    ZtB->>AppB: TCP 연결 전달<br/>(평문)
    Note right of AppB: 앱은 일반 TCP로 수신

    AppB-->>ZtB: TCP 응답
    ZtB-->>ZtA: HBONE 터널로 응답 전달
    ZtA-->>AppA: TCP 응답 전달
```

### 6.3 Ambient L7 흐름 (Waypoint 프록시 포함)

```mermaid
sequenceDiagram
    participant AppA as App A
    participant ZtA as ztunnel A<br/>(소스 노드)
    participant WP as Waypoint Proxy<br/>(L7 Envoy)
    participant ZtB as ztunnel B<br/>(대상 노드)
    participant AppB as App B

    Note over AppA,AppB: L7 정책 필요 시 waypoint 경유

    AppA->>ZtA: TCP 연결

    ZtA->>ZtA: 대상 서비스에<br/>waypoint 할당 확인
    Note right of ZtA: 서비스/네임스페이스에<br/>Gateway API로<br/>waypoint 설정됨

    ZtA->>WP: HBONE (포트 15008)
    Note over ZtA,WP: 1차 홉: ztunnel → waypoint<br/>mTLS + HBONE

    WP->>WP: L7 정책 처리
    Note right of WP: 1. HTTP 라우팅<br/>2. AuthorizationPolicy (L7)<br/>3. RequestAuthentication<br/>4. 트래픽 미러링/분할<br/>5. 재시도/타임아웃<br/>6. 헤더 조작

    WP->>ZtB: HBONE (포트 15008)
    Note over WP,ZtB: 2차 홉: waypoint → 대상 ztunnel<br/>mTLS + HBONE

    ZtB->>ZtB: 인바운드 정책 확인

    ZtB->>AppB: TCP 전달 (평문)

    AppB-->>ZtB: 응답
    ZtB-->>WP: HBONE 응답
    WP-->>ZtA: HBONE 응답
    ZtA-->>AppA: 응답 전달
```

### 6.4 HBONE 터널 프로토콜 구조

```
┌──────────────────────────────────────────────┐
│                  TCP 연결                     │
│  ┌──────────────────────────────────────────┐│
│  │              TLS (mTLS)                  ││
│  │  SPIFFE 인증서 기반 상호 인증             ││
│  │  ┌──────────────────────────────────────┐││
│  │  │         HTTP/2 프레임                │││
│  │  │  ┌──────────────────────────────────┐│││
│  │  │  │    CONNECT 메서드                ││││
│  │  │  │    :authority = target:port      ││││
│  │  │  │    ┌────────────────────────────┐││││
│  │  │  │    │    원본 TCP 페이로드       │││││
│  │  │  │    │    (앱 데이터)             │││││
│  │  │  │    └────────────────────────────┘││││
│  │  │  └──────────────────────────────────┘│││
│  │  └──────────────────────────────────────┘││
│  └──────────────────────────────────────────┘│
└──────────────────────────────────────────────┘

포트 할당:
- 15001: ztunnel 아웃바운드 (소스측 트래픽 가로채기)
- 15008: HBONE 터널 수신 포트 (mTLS over HTTP/2 CONNECT)
- 15006: 인바운드 (사이드카 모드에서 사용)
```

### 6.5 ztunnel의 xDS 리소스

ztunnel은 사이드카 Envoy와 다른 xDS 리소스 타입을 사용한다:

```
사이드카 Envoy가 사용하는 xDS:     ztunnel이 사용하는 xDS:
┌──────────────────────┐         ┌──────────────────────────┐
│ CDS (Cluster)        │         │ AddressType              │
│ EDS (Endpoint)       │         │ - 서비스 VIP 매핑         │
│ LDS (Listener)       │         │                          │
│ RDS (Route)          │         │ WorkloadType             │
│ SDS (Secret)         │         │ - Pod/VM 워크로드 정보    │
│                      │         │ - UID, IP, waypoint 연결  │
│                      │         │                          │
│                      │         │ WorkloadAuthorizationType│
│                      │         │ - L4 인가 정책            │
│                      │         │                          │
│                      │         │ SDS (Secret)             │
│                      │         │ - mTLS 인증서             │
└──────────────────────┘         └──────────────────────────┘
```

`PushOrder` (`ads.go:500-509`)에서 이러한 리소스 타입들의 순서가 정의되어 있다:

```go
var PushOrder = []string{
    v3.ClusterType,               // CDS
    v3.EndpointType,              // EDS
    v3.ListenerType,              // LDS
    v3.RouteType,                 // RDS
    v3.SecretType,                // SDS
    v3.AddressType,               // Ambient: 주소 매핑
    v3.WorkloadType,              // Ambient: 워크로드 정보
    v3.WorkloadAuthorizationType, // Ambient: L4 인가 정책
}
```

### 6.6 Ambient 모드 활성화 흐름

```mermaid
sequenceDiagram
    participant User as 사용자
    participant K8s as K8s API
    participant Ctrl as Controller
    participant AI as Ambient Index
    participant DS as DiscoveryServer
    participant Zt as ztunnel

    User->>K8s: 네임스페이스 라벨 설정<br/>istio.io/dataplane-mode=ambient

    K8s->>Ctrl: Namespace 이벤트 수신

    Ctrl->>AI: Ambient Index 업데이트
    Note right of AI: (ambient.New()로 초기화)<br/>controller.go:328-343<br/><br/>enableAmbient && ConfigCluster<br/>조건에서만 활성화

    AI->>AI: 워크로드 인덱스 재계산
    Note right of AI: 네임스페이스의 모든 Pod를<br/>Ambient 메시에 등록

    AI->>DS: ConfigUpdate(PushRequest)
    Note right of DS: WorkloadType,<br/>WorkloadAuthorizationType<br/>리소스 변경 포함

    DS->>Zt: xDS Push<br/>(AddressType + WorkloadType)
    Note right of Zt: ztunnel이 새 워크로드<br/>정보를 수신하여<br/>트래픽 가로채기 시작
```

---

## 흐름 간 관계 요약

다음 ASCII 다이어그램은 6개 핵심 흐름이 어떻게 연결되는지 보여준다:

```
    ┌─────────────────────────────────────────────────────────────────┐
    │                        Istiod (Control Plane)                  │
    │                                                                │
    │  ┌──────────────┐  ┌──────────────┐  ┌──────────────────┐     │
    │  │  Webhook     │  │ Service      │  │  CA (Citadel)    │     │
    │  │  Injection   │  │ Discovery    │  │  인증서 서명      │     │
    │  │  [흐름 3]    │  │ [흐름 4]     │  │  [흐름 2,5]      │     │
    │  └──────┬───────┘  └──────┬───────┘  └────────┬─────────┘     │
    │         │                  │                    │               │
    │         │           ┌──────▼───────┐           │               │
    │         │           │ Discovery    │           │               │
    │         │           │ Server       │           │               │
    │         │           │ xDS Push     │           │               │
    │         │           │ [흐름 1]     │           │               │
    │         │           └──────┬───────┘           │               │
    └─────────┼──────────────────┼───────────────────┼───────────────┘
              │                  │                    │
              │          gRPC xDS 스트림              │ gRPC CSR
              │                  │                    │
    ┌─────────▼──────────────────▼───────────────────▼───────────────┐
    │                     Data Plane                                  │
    │                                                                │
    │  ┌─────────────────────────────────────────────────────────┐   │
    │  │  사이드카 모드                                           │   │
    │  │  ┌──────────┐  mTLS [흐름 2]  ┌──────────┐             │   │
    │  │  │ Envoy A  │◄──────────────►│ Envoy B  │             │   │
    │  │  │ SDS 수신 │  인증서 로테이션│ SDS 수신 │             │   │
    │  │  │ [흐름 5] │  [흐름 5]      │ [흐름 5] │             │   │
    │  │  └──────────┘                └──────────┘             │   │
    │  └─────────────────────────────────────────────────────────┘   │
    │                                                                │
    │  ┌─────────────────────────────────────────────────────────┐   │
    │  │  Ambient 모드                                           │   │
    │  │  ┌──────────┐  HBONE [흐름 6] ┌──────────┐            │   │
    │  │  │ztunnel A │◄──────────────►│ztunnel B │            │   │
    │  │  └──────────┘                └──────────┘            │   │
    │  │         │          선택적 L7          │               │   │
    │  │         └──────►┌──────────┐◄────────┘               │   │
    │  │                 │ Waypoint │                          │   │
    │  │                 │ (Envoy)  │                          │   │
    │  │                 └──────────┘                          │   │
    │  └─────────────────────────────────────────────────────────┘   │
    └────────────────────────────────────────────────────────────────┘
```

---

## 핵심 설계 원칙 요약

| 원칙 | 적용 사례 | 이유 |
|------|----------|------|
| **디바운싱** | xDS 푸시 (100ms quiet, 10s max) | 대규모 설정 변경 시 과도한 푸시 방지 |
| **순서 보장** | PushOrder (CDS→EDS→LDS→RDS→SDS) | 의존 관계 충족, 일시적 장애 방지 |
| **투명 가로채기** | iptables REDIRECT (15001/15006) | 애플리케이션 무수정으로 mTLS 적용 |
| **Jitter** | 인증서 로테이션 시 +/- 1% | 대규모 플릿의 동시 갱신 부하 분산 |
| **HBONE 터널** | Ambient 모드 (HTTP/2 CONNECT over mTLS) | 사이드카 없이 L4 보안 제공 |
| **Lazy 로딩** | SDS on-demand 인증서 발급 | 불필요한 인증서 생성 방지 |
| **캐시 무효화** | xDS 캐시 Clear/ClearAll | 설정 변경 시 stale 응답 방지 |
