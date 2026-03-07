# Istio 코드 구조 분석

## 목차
1. [개요](#1-개요)
2. [전체 디렉토리 구조](#2-전체-디렉토리-구조)
3. [핵심 디렉토리 상세 설명](#3-핵심-디렉토리-상세-설명)
4. [빌드 시스템](#4-빌드-시스템)
5. [의존성 분석](#5-의존성-분석)
6. [테스트 구조](#6-테스트-구조)
7. [코드 구조 요약](#7-코드-구조-요약)

---

## 1. 개요

Istio는 Go 언어로 작성된 대규모 서비스 메시 프로젝트이다. 현재 분석 대상 버전은 **1.30**이며, 모듈 경로는 `istio.io/istio`이다. Go 1.25를 사용하며, 약 238줄에 달하는 `go.mod` 파일에서 알 수 있듯이 상당히 많은 외부 의존성을 가진다.

Istio의 코드는 기능별로 명확히 분리되어 있다. 컨트롤 플레인의 핵심인 `pilot/`, 공통 라이브러리 `pkg/`, 보안 기능 `security/`, 네트워크 플러그인 `cni/`, CLI 도구 `istioctl/`, 설치 관리자 `operator/`, 그리고 배포 매니페스트 `manifests/`가 주요 최상위 디렉토리이다.

```
주요 바이너리:
  - pilot-discovery : 컨트롤 플레인 (istiod)
  - pilot-agent     : 데이터 플레인 사이드카 에이전트
  - istioctl        : CLI 관리 도구
  - istio-cni       : CNI 플러그인
  - install-cni     : CNI 설치 바이너리
```

---

## 2. 전체 디렉토리 구조

```
istio/                              # 루트 (모듈: istio.io/istio, Go 1.25)
├── architecture/                   # 아키텍처 설계 문서
│   ├── ambient/                    #   Ambient 모드 설계
│   ├── environments/               #   환경별 설계
│   ├── networking/                 #   네트워킹 설계
│   ├── security/                   #   보안 설계
│   └── tests/                      #   테스트 설계
├── bin/                            # 초기화/빌드 헬퍼 스크립트
├── cni/                            # [컴포넌트] CNI 플러그인
│   ├── cmd/                        #   CNI 바이너리 진입점
│   │   ├── install-cni/            #     CNI 설치 바이너리
│   │   └── istio-cni/              #     CNI 플러그인 바이너리
│   ├── deployments/                #   Kubernetes 배포 매니페스트
│   ├── pkg/                        #   CNI 핵심 로직
│   └── test/                       #   CNI 테스트 데이터
├── common/                         # 공통 빌드 스크립트 (istio/common-files에서 동기화)
│   ├── config/                     #   린터/포매터 설정
│   └── scripts/                    #   빌드/배포 스크립트
├── docker/                         # Docker 이미지 설정
├── istioctl/                       # [컴포넌트] CLI 관리 도구
│   ├── cmd/                        #   istioctl 진입점
│   │   └── istioctl/               #     main.go
│   ├── docker/                     #   Docker 설정
│   └── pkg/                        #   istioctl 서브커맨드 구현
├── licenses/                       # 서드파티 라이선스
├── logo/                           # 프로젝트 로고
├── manifests/                      # [배포] Helm 차트 및 프로파일
│   ├── addons/                     #   애드온 (Prometheus, Grafana 등)
│   ├── charts/                     #   Helm 차트
│   ├── helm-profiles/              #   Helm values 프로파일
│   ├── profiles/                   #   IstioOperator 프로파일
│   └── sample-charts/              #   샘플 차트
├── operator/                       # [컴포넌트] Istio Operator
│   ├── cmd/                        #   Operator 커맨드
│   │   └── mesh/                   #     mesh 관리 커맨드
│   ├── pkg/                        #   Operator 핵심 로직
│   └── scripts/                    #   Operator 스크립트
├── pilot/                          # [컴포넌트] 컨트롤 플레인 핵심 (istiod)
│   ├── cmd/                        #   바이너리 진입점
│   │   ├── pilot-agent/            #     데이터 플레인 에이전트
│   │   └── pilot-discovery/        #     컨트롤 플레인 디스커버리
│   ├── docker/                     #   Docker 설정
│   ├── pkg/                        #   Pilot 핵심 로직
│   └── test/                       #   Pilot 테스트
├── pkg/                            # [공통] 공유 라이브러리
│   ├── config/                     #   설정 모델 및 스키마
│   ├── hbone/                      #   HBONE 프로토콜
│   ├── istio-agent/                #   Istio 에이전트 코어
│   ├── kube/                       #   Kubernetes 클라이언트/유틸리티
│   ├── model/                      #   프록시/xDS 모델
│   ├── security/                   #   보안 인터페이스
│   ├── test/                       #   테스트 프레임워크
│   ├── workloadapi/                #   Ambient 워크로드 API
│   └── ...                         #   기타 유틸리티 패키지
├── prow/                           # CI/CD (Prow) 설정
├── release/                        # 릴리스 관련 파일
├── releasenotes/                   # 릴리스 노트
├── samples/                        # 예제 애플리케이션
│   ├── bookinfo/                   #   BookInfo 샘플
│   ├── helloworld/                 #   HelloWorld 샘플
│   └── ...                         #   기타 샘플
├── security/                       # [컴포넌트] 보안 서브시스템
│   ├── pkg/                        #   보안 핵심 로직
│   └── tools/                      #   인증서 생성 도구
├── tests/                          # [테스트] 통합 테스트
│   ├── integration/                #   통합 테스트 스위트
│   ├── fuzz/                       #   퍼즈 테스트
│   └── common/                     #   테스트 공통 유틸리티
├── tools/                          # [도구] 개발/운영 도구
│   ├── bug-report/                 #   버그 리포트 도구
│   ├── istio-iptables/             #   iptables 규칙 관리
│   ├── istio-nftables/             #   nftables 규칙 관리
│   ├── docker-builder/             #   Docker 빌드 도구
│   └── proto/                      #   Protobuf 코드 생성
├── Makefile                        # 빌드 진입점 (Makefile.core.mk 위임)
├── Makefile.core.mk                # 실제 빌드 타겟 정의
├── go.mod                          # Go 모듈 정의
├── go.sum                          # 의존성 체크섬
└── VERSION                         # 버전 파일 (1.30)
```

---

## 3. 핵심 디렉토리 상세 설명

### 3.1 pilot/ -- 컨트롤 플레인 핵심

`pilot/`는 Istio 컨트롤 플레인의 심장부이다. istiod 바이너리의 대부분의 로직이 이곳에 구현되어 있다. 서비스 디스커버리, xDS 설정 생성, 보안 정책 적용 등 핵심 기능이 모두 포함된다.

#### 3.1.1 pilot/cmd/ -- 바이너리 진입점

```
pilot/cmd/
├── pilot-discovery/          # istiod 컨트롤 플레인 바이너리
│   ├── main.go               #   프로그램 진입점
│   └── app/
│       ├── cmd.go             #   cobra 루트 커맨드, discovery 서브커맨드 정의
│       ├── options.go         #   CLI 플래그/옵션 정의
│       └── request.go         #   디버그 요청 처리
└── pilot-agent/              # 사이드카 에이전트 바이너리
    ├── main.go               #   프로그램 진입점 (SDS 서버 초기화 포함)
    ├── app/
    │   ├── cmd.go             #   cobra 커맨드 (proxy, wait 등)
    │   ├── request.go         #   에이전트 요청 처리
    │   └── wait.go            #   Envoy 준비 대기 로직
    ├── config/                #   에이전트 설정
    ├── metrics/               #   에이전트 메트릭
    ├── options/               #   에이전트 옵션
    └── status/                #   상태 확인
```

**pilot-discovery**의 `main.go`는 매우 간결하다. `app.NewRootCommand()`를 호출하여 cobra 커맨드를 생성하고 실행한다. 실제 서버 초기화는 `pilot/pkg/bootstrap/`에서 수행된다.

```go
// pilot/cmd/pilot-discovery/main.go
func main() {
    log.EnableKlogWithCobra()
    rootCmd := app.NewRootCommand()
    if err := rootCmd.Execute(); err != nil {
        log.Error(err)
        os.Exit(-1)
    }
}
```

**pilot-agent**의 `main.go`는 SDS(Secret Discovery Service) 서버 팩토리를 주입하는 것이 특징이다. 빌드 태그(`agent`)를 통해 컨트롤 플레인 의존성을 제거하여 바이너리 크기를 최적화한다.

```go
// pilot/cmd/pilot-agent/main.go
func main() {
    log.EnableKlogWithCobra()
    rootCmd := app.NewRootCommand(
        func(options *security.Options, ...) istioagent.SDSService {
            return sds.NewServer(options, workloadSecretCache, pkpConf)
        })
    if err := rootCmd.Execute(); err != nil {
        log.Error(err)
        os.Exit(-1)
    }
}
```

#### 3.1.2 pilot/pkg/bootstrap/ -- 서버 부트스트랩

```
pilot/pkg/bootstrap/
├── server.go               # Server 구조체 정의, 핵심 초기화 흐름
├── discovery.go            # xDS 디스커버리 서버 초기화
├── configcontroller.go     # Config 컨트롤러 초기화 (CRD, 파일, 메모리)
├── servicecontroller.go    # 서비스 레지스트리 초기화 (Kube, ServiceEntry)
├── sidecarinjector.go      # 사이드카 인젝션 웹훅 설정
├── webhook.go              # Webhook 설정 (Validation, Mutation)
├── istio_ca.go             # 내장 CA (Citadel) 초기화
├── certcontroller.go       # 인증서 컨트롤러
├── mesh.go                 # MeshConfig 로딩/감시
├── monitoring.go           # 모니터링/메트릭 설정
├── options.go              # PilotArgs 구조체 (서버 설정 옵션)
├── validation.go           # 설정 검증 웹훅
├── util.go                 # 유틸리티 함수
├── config_compare.go       # 설정 비교 로직
└── *_test.go               # 각 파일별 단위 테스트
```

`server.go`는 istiod의 핵심이다. `Server` 구조체가 정의되어 있으며, 다음과 같은 순서로 초기화된다:

1. MeshConfig 로딩 (`mesh.go`)
2. Config 컨트롤러 초기화 (`configcontroller.go`)
3. 서비스 레지스트리 초기화 (`servicecontroller.go`)
4. xDS 디스커버리 서버 초기화 (`discovery.go`)
5. CA 서버 초기화 (`istio_ca.go`)
6. 사이드카 인젝터 초기화 (`sidecarinjector.go`)
7. Webhook 등록 (`webhook.go`)
8. gRPC/HTTP 서버 시작 (`server.go`)

#### 3.1.3 pilot/pkg/xds/ -- xDS 디스커버리 서비스

xDS는 Envoy 프록시의 동적 설정 프로토콜이며, Istio 컨트롤 플레인의 가장 중요한 기능이다. 이 패키지는 **49개의 Go 파일**로 구성되어 있다.

```
pilot/pkg/xds/
├── discovery.go            # DiscoveryServer 핵심 구조체, Start/Stop
├── ads.go                  # ADS(Aggregated Discovery Service) 스트림 핸들러
├── delta.go                # Delta xDS(증분 업데이트) 구현
├── pushqueue.go            # 설정 변경 시 Push 큐 관리
├── eventhandler.go         # 이벤트 핸들러 (Config/Service 변경 감지)
├── xdsgen.go               # xDS 응답 생성 공통 로직
├── krtxds.go               # KRT(Kubernetes Resource Transformer) 기반 xDS
│
├── cds.go                  # CDS (Cluster Discovery Service) 생성기
├── lds.go                  # LDS (Listener Discovery Service) 생성기
├── rds.go                  # RDS (Route Discovery Service) 생성기
├── eds.go                  # EDS (Endpoint Discovery Service) 생성기
├── sds.go                  # SDS (Secret Discovery Service) 생성기
├── nds.go                  # NDS (Name Discovery Service) -- DNS 프록시
├── ecds.go                 # ECDS (Extension Config Discovery Service)
├── pcds.go                 # PCDS (Proxy Config Discovery Service)
├── workload.go             # Ambient 모드 워크로드 xDS
│
├── auth.go                 # xDS 요청 인증/인가
├── proxy_dependencies.go   # 프록시별 의존성 관리 (어떤 설정을 push할지 결정)
├── monitoring.go           # xDS 메트릭
├── statusgen.go            # 상태 생성기
├── debug.go                # 디버그 엔드포인트 (/debug/*)
├── debuggen.go             # 디버그 응답 생성기
├── util.go                 # 유틸리티 함수
│
├── endpoints/              # 엔드포인트 관련 서브패키지
├── filters/                # Envoy 필터 상수 정의
├── requestidextension/     # 요청 ID 확장
├── v3/                     # xDS v3 프로토콜 상수
└── testdata/               # 테스트 데이터
```

xDS 타입별 생성기 파일 매핑:

| 파일 | xDS 타입 | 역할 |
|------|----------|------|
| `cds.go` | CDS | 업스트림 클러스터 설정 생성 |
| `lds.go` | LDS | 리스너 및 필터 체인 생성 |
| `rds.go` | RDS | HTTP 라우팅 규칙 생성 |
| `eds.go` | EDS | 서비스 엔드포인트 목록 생성 |
| `sds.go` | SDS | TLS 인증서/키 전달 |
| `nds.go` | NDS | DNS 이름 해석 테이블 전달 |
| `ecds.go` | ECDS | Wasm 등 확장 설정 전달 |
| `pcds.go` | PCDS | 프록시별 개별 설정 전달 |
| `workload.go` | WorkloadDS | Ambient 모드 워크로드 정보 전달 |

#### 3.1.4 pilot/pkg/networking/core/ -- Envoy 설정 생성

xDS 생성기가 호출하는 실제 Envoy 설정 빌더이다. **41개 Go 파일**로 구성되며, Envoy의 Listener, Cluster, Route 등 상세 설정을 Go 코드로 생성한다.

```
pilot/pkg/networking/core/
├── configgen.go                    # ConfigGenerator 인터페이스 및 구현
├── cluster.go                      # Envoy Cluster 설정 생성
├── cluster_builder.go              # Cluster 빌드 헬퍼
├── cluster_cache.go                # Cluster 캐시
├── cluster_tls.go                  # Cluster TLS 설정
├── cluster_traffic_policy.go       # Cluster 트래픽 정책 (CB, LB 등)
├── cluster_waypoint.go             # Waypoint 프록시용 Cluster
├── listener.go                     # Envoy Listener 설정 생성
├── listener_address.go             # Listener 주소 바인딩
├── listener_builder.go             # Listener 빌드 헬퍼
├── listener_inbound.go             # 인바운드 Listener (사이드카)
├── listener_waypoint.go            # Waypoint Listener
├── route.go                        # HTTP Route 설정 생성
├── route_cache.go                  # Route 캐시
├── gateway.go                      # 게이트웨이 설정 생성
├── accesslog.go                    # 액세스 로그 설정
├── filterchain_options.go          # 필터 체인 옵션
├── extension_config_builder.go     # 확장 설정 빌더
├── fake.go                         # 테스트용 Fake 구현
├── name_table.go                   # DNS 이름 테이블
├── networkfilter.go                # 네트워크 필터 설정
├── sidecar_simulation_test.go      # 시뮬레이션 테스트
└── ...                             # 기타 테스트 파일
```

`pilot/pkg/networking/`의 나머지 서브패키지:

```
pilot/pkg/networking/
├── core/                   # 핵심 Envoy 설정 생성 (위 설명)
├── apigen/                 # API Generator (커스텀 리소스 xDS)
├── grpcgen/                # gRPC 서비스 디스커버리 생성
├── plugin/                 # 네트워킹 플러그인 인터페이스
├── serviceentry/           # ServiceEntry 네트워킹 처리
├── telemetry/              # 텔레메트리 관련 필터 생성
└── util/                   # 네트워킹 유틸리티
```

#### 3.1.5 pilot/pkg/serviceregistry/ -- 서비스 레지스트리

서비스 디스커버리의 핵심이다. Kubernetes, ServiceEntry 등 다양한 소스로부터 서비스 정보를 수집한다.

```
pilot/pkg/serviceregistry/
├── instance.go                     # ServiceInstance 인터페이스
├── serviceregistry_test.go         # 통합 테스트
├── aggregate/                      # 여러 레지스트리를 집약하는 컨트롤러
├── kube/                           # Kubernetes 기반 레지스트리
│   ├── controller/                 #   Kubernetes 컨트롤러 (Service, Endpoints 감시)
│   │   └── ambient/                #     Ambient 모드 전용 컨트롤러
│   ├── conversion.go               #   K8s 리소스 -> Istio 모델 변환
│   └── testdata/                   #   테스트 데이터
├── memory/                         # 인메모리 레지스트리 (테스트용)
├── mock/                           # Mock 레지스트리 (테스트용)
├── provider/                       # 레지스트리 프로바이더 ID 정의
├── serviceentry/                   # ServiceEntry CRD 기반 레지스트리
└── util/                           # 유틸리티
```

`aggregate/` 패키지가 핵심이다. 여러 레지스트리(Kube, ServiceEntry, Memory 등)를 하나의 통합 인터페이스로 제공하여, xDS 생성기가 모든 서비스를 일관되게 조회할 수 있게 한다.

#### 3.1.6 pilot/pkg/model/ -- 핵심 데이터 모델

```
pilot/pkg/model/
├── config.go               # Config 저장소 인터페이스 (ConfigStore, ConfigStoreController)
├── context.go              # Proxy 컨텍스트 (Sidecar/Gateway 판별, 메타데이터)
├── controller.go           # 컨트롤러 인터페이스
├── authentication.go       # 인증 정책 모델
├── authorization.go        # 인가 정책 모델
├── destination_rule.go     # DestinationRule 모델
├── envoyfilter.go          # EnvoyFilter 모델
├── endpointshards.go       # 엔드포인트 샤드 (다중 클러스터 지원)
├── addressmap.go           # 주소 매핑 (VIP -> 엔드포인트)
├── cluster_local.go        # 클러스터 로컬 설정
├── push_context.go         # PushContext (xDS 생성 시점의 스냅샷)
├── sidecar.go              # Sidecar CRD 모델
├── service.go              # Service, Port, ServiceInstance 정의
├── credentials/            # 인증서 관리 인터페이스
├── kstatus/                # Kubernetes 상태 관리
├── status/                 # 일반 상태 관리
└── test/                   # 테스트 헬퍼
```

`PushContext`는 특히 중요한 구조체이다. xDS 설정을 생성하는 시점에 필요한 모든 정보(서비스 목록, 정책, 인증서 등)를 담는 스냅샷 역할을 한다. 설정 변경이 발생할 때마다 새로운 `PushContext`가 생성되어 일관된 상태에서 Envoy 설정을 만들 수 있게 한다.

#### 3.1.7 pilot/pkg/config/ -- 설정 관리

```
pilot/pkg/config/
├── aggregate/              # 여러 Config 소스를 집약
├── file/                   # 파일 기반 Config 소스 (MCP, 로컬 파일)
├── kube/                   # Kubernetes CRD 기반 Config 소스
└── memory/                 # 인메모리 Config 소스 (테스트용)
```

설정 소스의 추상화 계층이다. Kubernetes CRD, 로컬 파일, 메모리 등 다양한 소스로부터 Istio 설정(VirtualService, DestinationRule 등)을 읽어온다. `aggregate/`를 통해 여러 소스를 하나로 합치는 패턴은 서비스 레지스트리와 동일하다.

### 3.2 pkg/ -- 공통 라이브러리

프로젝트 전반에서 공유되는 패키지이다. pilot, security, cni, istioctl 등 모든 컴포넌트가 이 패키지들을 사용한다.

#### 3.2.1 pkg/config/ -- 공통 설정 모델

```
pkg/config/
├── model.go                # Config 구조체 (Meta + Spec), GroupVersionKind 정의
├── conversion.go           # 설정 변환 유틸리티
├── doc.go                  # 패키지 문서
├── analysis/               # 설정 분석/검증 엔진 (istioctl analyze)
│   ├── analyzers/          #   개별 분석기 (IST0xxx 에러 코드)
│   ├── diag/               #   진단 메시지 정의
│   ├── local/              #   로컬 분석
│   └── msg/                #   메시지 포매팅
├── constants/              # 공통 상수
├── crd/                    # CRD 변환
├── gateway/                # Gateway API 변환
│   └── kube/               #   K8s Gateway API -> Istio 내부 모델 변환
├── host/                   # 호스트명 매칭 로직
├── labels/                 # 레이블 매칭
├── mesh/                   # MeshConfig 관리
│   ├── kubemesh/           #   K8s ConfigMap 기반 MeshConfig
│   └── meshwatcher/        #   MeshConfig 변경 감시
├── protocol/               # 프로토콜 식별 (HTTP, TCP, gRPC 등)
├── resource/               # 리소스 이름/네임스페이스 타입
├── schema/                 # Istio 리소스 스키마 정의
│   ├── collection/         #   스키마 컬렉션
│   ├── collections/        #   빌트인 컬렉션 목록 (code-gen)
│   ├── gvk/                #   GroupVersionKind 상수
│   ├── gvr/                #   GroupVersionResource 상수
│   ├── kind/               #   Kind 상수
│   ├── kubeclient/         #   K8s 클라이언트 팩토리
│   └── kubetypes/          #   K8s 타입 매핑
├── security/               # 보안 관련 설정 상수
├── validation/             # 설정 유효성 검증
│   ├── agent/              #   에이전트 측 검증
│   └── envoyfilter/        #   EnvoyFilter 검증
├── visibility/             # 리소스 가시성 제어
└── xds/                    # xDS 리소스 타입 상수
```

#### 3.2.2 pkg/model/ -- 프록시 모델

```
pkg/model/
├── proxy.go                # NodeType, NodeMetadata 등 프록시 모델
├── xds.go                  # xDS 리소스 타입 상수 (ClusterType, ListenerType 등)
├── authentication.go       # 인증 관련 상수
├── fips.go                 # FIPS 모드 지원
└── wasm.go                 # Wasm 모듈 모델
```

`pilot/pkg/model/`이 Istio 내부의 상세 데이터 모델이라면, `pkg/model/`은 프록시 노드 타입, xDS 리소스 타입 등 보다 기초적인 모델을 정의한다.

#### 3.2.3 pkg/security/ -- 보안 인터페이스

```
pkg/security/
├── security.go             # Options, SecretManager 인터페이스, SecretItem 구조체
├── authentication.go       # Authenticator 인터페이스
├── mock.go                 # Mock 구현
└── retry.go                # 인증서 요청 재시도 로직
```

보안 관련 공통 인터페이스와 타입을 정의한다. `SecretManager`는 인증서 캐싱과 갱신을 관리하는 핵심 인터페이스이며, `security/pkg/nodeagent/cache/`에서 실제로 구현된다.

#### 3.2.4 pkg/kube/ -- Kubernetes 클라이언트/유틸리티

```
pkg/kube/
├── inject/                 # 사이드카 인젝션 구현
│   ├── inject.go           #   인젝션 로직 (Pod 템플릿 변환)
│   ├── initializer.go      #   인젝션 초기화
│   ├── app_probe.go        #   앱 프로브 재작성
│   ├── monitoring.go       #   인젝션 메트릭
│   └── openshift.go        #   OpenShift 호환성
├── controllers/            # 범용 K8s 컨트롤러 유틸리티
├── informerfactory/        # Informer 팩토리 래퍼
├── kclient/                # K8s 클라이언트 추상화
│   └── clienttest/         #   테스트 유틸리티
├── krt/                    # KRT (Kubernetes Resource Transformer)
│   ├── files/              #   파일 기반 리소스
│   └── krttest/            #   KRT 테스트 유틸리티
├── kubetypes/              # K8s 타입 정의
├── labels/                 # 레이블 관리
├── mcs/                    # Multi-Cluster Service API
├── multicluster/           # 다중 클러스터 지원
├── namespace/              # 네임스페이스 필터링
├── watcher/                # 리소스 감시자
│   └── configmapwatcher/   #   ConfigMap 감시
└── apimirror/              # API 미러링
```

**pkg/kube/inject/**는 사이드카 자동 인젝션의 핵심 구현이다. MutatingWebhookConfiguration을 통해 Pod 생성 시 자동으로 Envoy 사이드카 컨테이너를 주입한다.

**pkg/kube/krt/**는 비교적 최근에 도입된 KRT(Kubernetes Resource Transformer) 프레임워크이다. Kubernetes 리소스를 선언적으로 변환하고 캐싱하는 반응형 프로그래밍 모델을 제공한다. 기존의 Informer 기반 패턴보다 간결하고 테스트하기 쉬운 코드를 작성할 수 있게 해준다.

#### 3.2.5 pkg/hbone/ -- HBONE 프로토콜

```
pkg/hbone/
├── dialer.go               # HBONE 다이얼러 (클라이언트 측)
├── doubledialer.go         # 이중 다이얼러 (fallback 지원)
├── server.go               # HBONE 서버 (프록시 측)
└── util.go                 # 유틸리티
```

HBONE(HTTP-Based Overlay Network Encapsulation)은 Istio Ambient 모드에서 사용하는 터널링 프로토콜이다. HTTP/2 CONNECT 메서드를 활용하여 TCP 트래픽을 mTLS로 암호화된 HTTP/2 터널을 통해 전달한다.

#### 3.2.6 pkg/workloadapi/ -- Ambient 워크로드 API

```
pkg/workloadapi/
├── workload.pb.go              # Protobuf 생성 코드
├── workload_vtproto.pb.go      # vtprotobuf 최적화 코드
├── workload_json.gen.go        # JSON 시리얼라이제이션
└── security/                   # 워크로드 보안 관련 Protobuf
```

Ambient 모드에서 ztunnel과 waypoint 프록시가 워크로드 정보를 수신하기 위한 API를 정의한다. 이 API를 통해 Pod IP, 서비스 매핑, 정책 정보 등이 전달된다.

#### 3.2.7 pkg/istio-agent/ -- Istio 에이전트 코어

```
pkg/istio-agent/
├── agent.go                # Agent 구조체, Start/Stop 메서드
├── xds_proxy.go            # xDS 프록시 (istiod <-> Envoy 사이 프록시)
├── xds_proxy_delta.go      # Delta xDS 프록시
├── plugins.go              # 에이전트 플러그인
├── grpcxds/                # gRPC xDS 클라이언트
├── health/                 # 헬스 체크
└── metrics/                # 에이전트 메트릭
```

`pilot-agent` 바이너리의 핵심 로직이다. 사이드카 컨테이너 내에서 실행되며, istiod와 Envoy 프록시 사이의 xDS 프록시 역할을 한다. 또한 SDS를 통해 인증서를 관리하고, 헬스 체크를 수행한다.

### 3.3 security/ -- 보안 서브시스템

```
security/
├── pkg/
│   ├── pki/                        # PKI 인프라
│   │   ├── ca/                     #   CA 구현 (자체 서명, 외부 CA)
│   │   │   ├── ca.go               #     IstioCA 구현 (인증서 발급)
│   │   │   └── selfsignedcarootcertrotator.go  # 루트 인증서 자동 회전
│   │   ├── ra/                     #   Registration Authority
│   │   ├── util/                   #   인증서 유틸리티
│   │   └── error/                  #   PKI 에러 정의
│   ├── nodeagent/                  # 노드 에이전트 (사이드카 측)
│   │   ├── sds/                    #   SDS 서버 구현
│   │   │   ├── server.go           #     SDS gRPC 서버
│   │   │   └── sdsservice.go       #     SDS 서비스 핸들러
│   │   ├── cache/                  #   인증서 캐시 (SecretManager 구현)
│   │   ├── caclient/               #   CA 클라이언트 (인증서 요청)
│   │   ├── cafile/                 #   파일 기반 CA
│   │   └── util/                   #   유틸리티
│   ├── server/                     # CA 서버
│   │   └── ca/                     #   CA gRPC 서비스 구현
│   ├── k8s/                        # Kubernetes 보안 연동
│   │   ├── chiron/                 #   K8s CertificateSigningRequest 통합
│   │   ├── controller/             #   보안 컨트롤러
│   │   └── tokenreview/            #   토큰 검증
│   ├── credentialfetcher/          # 자격 증명 가져오기
│   │   └── plugin/                 #   플러그인 (GCE 등)
│   ├── cmd/                        # 보안 관련 CLI 커맨드
│   ├── monitoring/                 # 보안 메트릭
│   └── util/                      # 보안 유틸리티
├── samples/                        # 보안 샘플 (인증서 예시)
└── tools/                          # 인증서 생성 도구
    ├── generate_cert/              #   인증서 생성기
    ├── generate_csr/               #   CSR 생성기
    └── jwt/                        #   JWT 테스트 도구
```

보안 서브시스템의 핵심 흐름:

```
[워크로드 Pod]                           [istiod]
pilot-agent                              pilot-discovery
  │                                        │
  ├── nodeagent/sds/server.go             ├── bootstrap/istio_ca.go
  │    (SDS gRPC 서버)                     │    (IstioCA 초기화)
  │         │                              │
  ├── nodeagent/cache/                     ├── pki/ca/ca.go
  │    (SecretManager)                     │    (인증서 발급)
  │         │                              │
  └── nodeagent/caclient/                  └── server/ca/
       (CSR 전송) ─────── gRPC ──────────>      (CSR 처리)
```

### 3.4 cni/ -- CNI 플러그인

```
cni/
├── cmd/
│   ├── istio-cni/              # CNI 플러그인 바이너리 (kubelet이 호출)
│   └── install-cni/            # CNI 설치 바이너리 (DaemonSet으로 배포)
├── pkg/
│   ├── plugin/                 # CNI 플러그인 로직 (Pod 네트워크 설정)
│   ├── install/                # CNI 설치 로직 (설정 파일 배포)
│   ├── config/                 # CNI 설정
│   ├── constants/              # 상수 정의
│   ├── iptables/               # iptables 규칙 설정
│   ├── nftables/               # nftables 규칙 설정
│   ├── ipset/                  # IP 집합 관리
│   ├── addressset/             # 주소 집합 관리
│   ├── nodeagent/              # 노드 에이전트 (Ambient 모드)
│   ├── repair/                 # Pod 복구 (init 컨테이너 실패 시)
│   ├── trafficmanager/         # 트래픽 관리자
│   ├── pluginlistener/         # 플러그인 리스너
│   ├── log/                    # CNI 로깅
│   ├── monitoring/             # CNI 메트릭
│   ├── scopes/                 # 로그 스코프
│   └── util/                   # 유틸리티
├── deployments/                # K8s 배포 매니페스트
│   └── kubernetes/             #   DaemonSet 정의
└── test/                       # CNI 테스트
    └── testdata/               #   테스트 데이터
```

CNI 플러그인은 두 가지 모드로 동작한다:
- **사이드카 모드**: Pod에 init 컨테이너 대신 CNI 플러그인이 iptables 규칙을 설정
- **Ambient 모드**: ztunnel로의 트래픽 리다이렉션을 노드 수준에서 설정

### 3.5 istioctl/ -- CLI 관리 도구

```
istioctl/
├── cmd/
│   └── istioctl/               # main.go 진입점
├── pkg/
│   ├── admin/                  # istioctl admin 커맨드
│   ├── analyze/                # istioctl analyze (설정 분석)
│   ├── authz/                  # 인가 정책 검사
│   ├── checkinject/            # 인젝션 상태 확인
│   ├── cli/                    # CLI 공통 유틸리티
│   ├── clioptions/             # CLI 옵션 정의
│   ├── completion/             # 자동 완성
│   ├── config/                 # 설정 관리
│   ├── dashboard/              # 대시보드 열기
│   ├── describe/               # 리소스 상세 설명
│   ├── injector/               # 인젝터 상태 확인
│   ├── install/                # 설치 커맨드
│   │   └── k8sversion/         #   K8s 버전 호환성 확인
│   ├── internaldebug/          # 내부 디버그
│   ├── kubeinject/             # 수동 사이드카 인젝션
│   ├── metrics/                # 메트릭 조회
│   ├── multicluster/           # 다중 클러스터 관리
│   ├── multixds/               # 다중 xDS 쿼리
│   ├── precheck/               # 설치 사전 점검
│   ├── proxyconfig/            # 프록시 설정 조회/변경
│   ├── proxystatus/            # 프록시 상태 조회
│   ├── root/                   # 루트 커맨드
│   ├── tag/                    # 리비전 태그 관리
│   ├── validate/               # 설정 유효성 검증
│   ├── version/                # 버전 정보
│   ├── waypoint/               # Waypoint 프록시 관리
│   ├── workload/               # 워크로드 관리
│   ├── writer/                 # 출력 포매팅
│   │   ├── compare/            #   설정 비교 출력
│   │   ├── envoy/              #   Envoy 설정 출력
│   │   ├── pilot/              #   Pilot 설정 출력
│   │   ├── table/              #   테이블 포매팅
│   │   └── ztunnel/            #   ztunnel 설정 출력
│   ├── xds/                    # xDS 직접 쿼리
│   └── ztunnelconfig/          # ztunnel 설정 조회
└── docker/                     # Docker 설정
```

주요 `istioctl` 커맨드와 대응 패키지:

| 커맨드 | 패키지 | 기능 |
|--------|--------|------|
| `istioctl analyze` | `analyze/` | 설정 분석 및 문제 감지 |
| `istioctl install` | `install/` | Istio 설치 |
| `istioctl kube-inject` | `kubeinject/` | 수동 사이드카 인젝션 |
| `istioctl proxy-config` | `proxyconfig/` | Envoy 프록시 설정 조회 |
| `istioctl proxy-status` | `proxystatus/` | 프록시 동기화 상태 확인 |
| `istioctl dashboard` | `dashboard/` | Kiali, Grafana 등 대시보드 열기 |
| `istioctl validate` | `validate/` | YAML 유효성 검사 |
| `istioctl waypoint` | `waypoint/` | Waypoint 프록시 생성/삭제 |
| `istioctl tag` | `tag/` | 리비전 태그 관리 |

### 3.6 operator/ -- Istio Operator

```
operator/
├── cmd/
│   └── mesh/                   # mesh 관리 커맨드 (install, manifest, profile)
├── pkg/
│   ├── apis/                   # IstioOperator API 정의
│   │   └── validation/         #   API 유효성 검증
│   ├── component/              # 컴포넌트 (pilot, cni, ztunnel 등) 관리
│   ├── helm/                   # Helm 렌더링 엔진
│   ├── install/                # 설치 로직
│   ├── manifest/               # 매니페스트 생성
│   ├── render/                 # 렌더링 엔진
│   ├── tpath/                  # 트리 경로 유틸리티
│   ├── uninstall/              # 삭제 로직
│   ├── util/                   # 유틸리티
│   ├── values/                 # Helm values 처리
│   ├── version/                # 버전 관리
│   └── webhook/                # Operator 웹훅
├── scripts/                    # 운영 스크립트
└── version/                    # 버전 정보
```

Operator는 `IstioOperator` CRD를 통해 Istio의 선언적 설치/업그레이드를 지원한다. 내부적으로 Helm 차트를 렌더링하여 Kubernetes 리소스를 생성한다.

### 3.7 tools/ -- 개발/운영 도구

```
tools/
├── istio-iptables/             # iptables 트래픽 리다이렉션
│   └── pkg/
│       ├── builder/            #   iptables 규칙 빌더
│       ├── capture/            #   트래픽 캡처 설정
│       ├── cmd/                #   CLI 진입점
│       ├── constants/          #   상수 (포트, 체인 이름)
│       ├── dependencies/       #   시스템 의존성 (iptables 바이너리)
│       └── validation/         #   규칙 검증
├── istio-nftables/             # nftables 기반 (iptables 대안)
│   └── pkg/
│       ├── builder/            #   nftables 규칙 빌더
│       ├── capture/            #   트래픽 캡처 설정
│       ├── constants/          #   상수
│       └── nft/                #   nft 명령 래퍼
├── bug-report/                 # 버그 리포트 수집 도구
│   └── pkg/
│       ├── bugreport/          #   리포트 생성 로직
│       ├── cluster/            #   클러스터 정보 수집
│       ├── content/            #   컨텐츠 수집기
│       ├── filter/             #   필터링
│       └── kubectlcmd/         #   kubectl 명령 래퍼
├── certs/                      # 인증서 생성 스크립트
├── docker-builder/             # Docker 이미지 빌드 도구
│   ├── builder/                #   빌드 로직
│   └── dockerfile/             #   Dockerfile 생성
├── proto/                      # Protobuf 코드 생성 Makefile
└── packaging/                  # 패키징 도구
    └── common/                 #   공통 패키징 스크립트
```

`tools/istio-iptables/`는 사이드카 init 컨테이너에서 실행되어 Pod의 iptables 규칙을 설정한다. 모든 인/아웃바운드 트래픽을 Envoy 프록시로 리다이렉트하는 REDIRECT/TPROXY 규칙을 생성한다. `tools/istio-nftables/`는 nftables 기반의 대안 구현이다.

### 3.8 manifests/ -- Helm 차트 및 프로파일

```
manifests/
├── charts/
│   ├── base/                   # CRD 및 기본 리소스
│   │   ├── files/              #   CRD YAML 파일
│   │   └── templates/          #   Helm 템플릿
│   ├── default/                # 기본 설정 (istiod가 사용)
│   │   ├── files/              #   기본 파일
│   │   └── templates/          #   기본 템플릿
│   ├── istio-control/
│   │   └── istio-discovery/    # istiod 차트 (Deployment, Service 등)
│   ├── gateway/                # Gateway 차트 (Ingress/Egress)
│   │   ├── files/              #   게이트웨이 파일
│   │   └── templates/          #   게이트웨이 템플릿
│   ├── gateways/
│   │   ├── istio-ingress/      # Ingress Gateway 차트
│   │   └── istio-egress/       # Egress Gateway 차트
│   ├── istio-cni/              # CNI 플러그인 차트
│   │   ├── files/              #   CNI 파일
│   │   └── templates/          #   CNI 템플릿
│   └── ztunnel/                # ztunnel 차트 (Ambient 모드)
│       ├── files/              #   ztunnel 파일
│       └── templates/          #   ztunnel 템플릿
├── helm-profiles/              # Helm values 프로파일
│   ├── ambient.yaml            #   Ambient 모드 프로파일
│   ├── demo.yaml               #   데모 프로파일 (모든 기능 활성화)
│   ├── stable.yaml             #   안정 프로파일
│   ├── preview.yaml            #   미리보기 프로파일
│   ├── remote.yaml             #   원격 클러스터 프로파일
│   ├── platform-gke.yaml       #   GKE 플랫폼 프로파일
│   ├── platform-minikube.yaml  #   Minikube 프로파일
│   ├── platform-openshift.yaml #   OpenShift 프로파일
│   └── ...                     #   기타 호환성/플랫폼 프로파일
├── profiles/                   # IstioOperator 프로파일
│   ├── default.yaml            #   기본 프로파일
│   ├── demo.yaml               #   데모 프로파일
│   ├── minimal.yaml            #   최소 설치 프로파일
│   ├── ambient.yaml            #   Ambient 모드 프로파일
│   ├── remote.yaml             #   원격 클러스터 프로파일
│   └── ...                     #   기타 프로파일
├── addons/                     # 애드온 (Prometheus, Grafana, Jaeger, Kiali)
│   └── dashboards/             #   Grafana 대시보드 JSON
└── sample-charts/              # 샘플 차트
    └── ambient/                #   Ambient 모드 샘플
```

`helm-profiles/`과 `profiles/`의 차이:
- `helm-profiles/`: Helm `values.yaml` 오버라이드 형식 (Helm 네이티브 설치에 사용)
- `profiles/`: `IstioOperator` CRD 형식 (Operator/istioctl을 통한 설치에 사용)

---

## 4. 빌드 시스템

### 4.1 빌드 파일 구조

```
Makefile                    # 진입점 -- BUILD_WITH_CONTAINER 분기
Makefile.core.mk            # 실제 빌드 타겟 정의
Makefile.overrides.mk       # (선택) 사용자별 오버라이드
common/scripts/
├── setup_env.sh            # 환경 변수 설정
├── gobuild.sh              # Go 바이너리 빌드 스크립트
└── run.sh                  # 컨테이너 내 빌드 실행
go.mod                      # Go 모듈 정의 (istio.io/istio)
go.sum                      # 의존성 체크섬
tools/proto/proto.mk        # Protobuf 코드 생성 Makefile
VERSION                     # 버전 파일 (1.30)
```

### 4.2 Makefile 구조

Istio는 2단계 Makefile 구조를 사용한다:

```
Makefile (진입점)
  │
  ├── BUILD_WITH_CONTAINER=1 일 때
  │     → Docker 컨테이너 안에서 Makefile.core.mk 실행
  │
  └── BUILD_WITH_CONTAINER=0 일 때 (기본값)
        → common/scripts/setup_env.sh로 환경 설정
        → Makefile.core.mk 직접 실행
```

이 구조 덕분에 로컬 개발 환경과 CI/CD 환경(Docker 컨테이너)에서 동일한 빌드 결과를 보장한다.

### 4.3 주요 Makefile 타겟

| 타겟 | 설명 |
|------|------|
| `make build` | 모든 Go 바이너리 빌드 (로컬 OS/아키텍처) |
| `make build-linux` | Linux 바이너리 빌드 (컨테이너 이미지용) |
| `make build-cni` | CNI 바이너리만 빌드 |
| `make test` | 모든 단위 테스트 실행 (`racetest`와 동일) |
| `make racetest` | Race detector 활성화 단위 테스트 |
| `make benchtest` | 벤치마크 테스트 |
| `make lint` | 모든 린터 실행 (Go, Python, YAML, Helm 등) |
| `make format` / `make fmt` | 코드 포매팅 (gofmt, Python, go mod tidy) |
| `make precommit` | 커밋 전 체크 (format + lint) |
| `make gen` | 코드 생성 (protobuf, CRD 등) |
| `make gen-check` | 코드 생성 결과 검증 |
| `make clean` | 빌드 산출물 삭제 |
| `make init` | 빌드 의존성 초기화 (Envoy 바이너리 다운로드) |
| `make push` | Docker 이미지 빌드 및 푸시 |
| `make istioctl-all` | 모든 플랫폼용 istioctl 크로스 컴파일 |

### 4.4 바이너리 분류 및 빌드 태그

Istio는 바이너리를 세 가지 범주로 분류한다:

```makefile
# 표준 바이너리 (빌드 태그: vtprotobuf,disable_pgv)
STANDARD_BINARIES := ./istioctl/cmd/istioctl \
  ./pilot/cmd/pilot-discovery \
  ./pkg/test/echo/cmd/client \
  ./pkg/test/echo/cmd/server \
  ./samples/extauthz/cmd/extauthz

# 에이전트 바이너리 (빌드 태그: agent,disable_pgv,grpcnotrace,retrynotrace)
AGENT_BINARIES := ./pilot/cmd/pilot-agent

# Linux 전용 에이전트 바이너리
LINUX_AGENT_BINARIES := ./cni/cmd/istio-cni \
  ./cni/cmd/install-cni \
  $(AGENT_BINARIES)
```

빌드 태그의 의미:

| 빌드 태그 | 설명 |
|-----------|------|
| `agent` | 에이전트 전용 파일 활성화, 불필요한 의존성 제거 |
| `vtprotobuf` | 최적화된 Protobuf 직렬화 (표준 바이너리만) |
| `disable_pgv` | protoc-gen-validate 비활성화 (바이너리 크기 절감) |
| `grpcnotrace` | gRPC 트레이스 비활성화 |
| `retrynotrace` | 재시도 트레이스 비활성화 |

이 분류의 핵심 이유는 **바이너리 크기 최적화**이다. `pilot-agent`는 모든 사이드카 Pod에서 실행되므로, 컨트롤 플레인(xDS 서버, K8s 클라이언트 등)의 무거운 의존성을 빌드 태그로 제외하여 크기를 줄인다.

### 4.5 빌드 흐름

```
make build
  │
  ├── make depend (init)
  │     ├── bin/init.sh               # Envoy 바이너리 다운로드
  │     └── bin/build_ztunnel.sh      # ztunnel 빌드
  │
  ├── common/scripts/gobuild.sh       # Go 빌드 스크립트
  │     ├── STANDARD_BINARIES         # -tags=vtprotobuf,disable_pgv
  │     └── AGENT_BINARIES            # -tags=agent,disable_pgv,...
  │
  └── 출력: $(TARGET_OUT)/
        ├── pilot-discovery
        ├── pilot-agent
        ├── istioctl
        └── ...
```

---

## 5. 의존성 분석

### 5.1 핵심 의존성

`go.mod`에 정의된 주요 직접 의존성을 기능별로 분류한다.

#### Envoy/xDS 관련

| 패키지 | 역할 |
|--------|------|
| `github.com/cncf/xds/go` | xDS 프로토콜 타입 정의 |
| `github.com/envoyproxy/go-control-plane/envoy` | Envoy API v3 타입 (Cluster, Listener 등) |
| `github.com/envoyproxy/go-control-plane/contrib` | Envoy contrib 확장 타입 |
| `github.com/envoyproxy/protoc-gen-validate` | Protobuf 검증 (간접 의존성) |
| `github.com/planetscale/vtprotobuf` | 최적화된 Protobuf 직렬화 |

#### Kubernetes 관련

| 패키지 | 역할 |
|--------|------|
| `k8s.io/client-go` | K8s API 클라이언트 |
| `k8s.io/api` | K8s 핵심 API 타입 |
| `k8s.io/apimachinery` | K8s 메타 타입 |
| `k8s.io/apiextensions-apiserver` | CRD 관련 타입 |
| `k8s.io/cli-runtime` | kubectl 호환 CLI 런타임 |
| `k8s.io/kubectl` | kubectl 라이브러리 |
| `sigs.k8s.io/controller-runtime` | 컨트롤러 프레임워크 |
| `sigs.k8s.io/gateway-api` | Gateway API 타입 |
| `sigs.k8s.io/mcs-api` | Multi-Cluster Service API |

#### gRPC/Protobuf 관련

| 패키지 | 역할 |
|--------|------|
| `google.golang.org/grpc` | gRPC 프레임워크 |
| `google.golang.org/protobuf` | Protobuf v2 런타임 |
| `github.com/golang/protobuf` | Protobuf v1 호환 |
| `github.com/gogo/protobuf` | GoGo Protobuf (레거시) |
| `github.com/grpc-ecosystem/go-grpc-prometheus` | gRPC 메트릭 |
| `github.com/grpc-ecosystem/go-grpc-middleware/v2` | gRPC 미들웨어 |

#### Istio API 관련

| 패키지 | 역할 |
|--------|------|
| `istio.io/api` | Istio API 타입 (VirtualService, DestinationRule 등) |
| `istio.io/client-go` | Istio API K8s 클라이언트 |

#### CLI 관련

| 패키지 | 역할 |
|--------|------|
| `github.com/spf13/cobra` | CLI 프레임워크 |
| `github.com/spf13/pflag` | POSIX 호환 플래그 |
| `github.com/spf13/viper` | 설정 관리 |

#### 네트워킹 관련

| 패키지 | 역할 |
|--------|------|
| `github.com/containernetworking/cni` | CNI 표준 인터페이스 |
| `github.com/containernetworking/plugins` | CNI 표준 플러그인 |
| `github.com/miekg/dns` | DNS 라이브러리 |
| `github.com/vishvananda/netlink` | 리눅스 네트워크 인터페이스 |
| `github.com/vishvananda/netns` | 리눅스 네트워크 네임스페이스 |
| `github.com/pires/go-proxyproto` | PROXY 프로토콜 |
| `github.com/quic-go/quic-go` | QUIC 프로토콜 |
| `sigs.k8s.io/knftables` | nftables 라이브러리 |

#### 모니터링/관찰성

| 패키지 | 역할 |
|--------|------|
| `github.com/prometheus/client_golang` | Prometheus 클라이언트 |
| `go.opentelemetry.io/otel` | OpenTelemetry SDK |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace` | OTLP 트레이스 익스포터 |
| `go.opentelemetry.io/otel/exporters/prometheus` | Prometheus 익스포터 |
| `go.uber.org/zap` | 구조화된 로깅 |

#### 보안 관련

| 패키지 | 역할 |
|--------|------|
| `github.com/coreos/go-oidc/v3` | OpenID Connect |
| `github.com/go-jose/go-jose/v4` | JOSE (JWT/JWE/JWS) |
| `github.com/lestrrat-go/jwx` | JWT 처리 |
| `github.com/google/cel-go` | CEL (Common Expression Language) |
| `github.com/spiffe/go-spiffe/v2` | SPIFFE 신원 프레임워크 |

#### 기타 유틸리티

| 패키지 | 역할 |
|--------|------|
| `helm.sh/helm/v3` | Helm 라이브러리 (차트 렌더링) |
| `github.com/fsnotify/fsnotify` | 파일 시스템 감시 |
| `github.com/google/go-containerregistry` | 컨테이너 레지스트리 클라이언트 |
| `github.com/hashicorp/go-multierror` | 다중 에러 처리 |
| `github.com/hashicorp/golang-lru/v2` | LRU 캐시 |
| `github.com/yl2chen/cidranger` | CIDR 범위 매칭 |
| `github.com/gorilla/mux` | HTTP 라우터 |
| `github.com/gorilla/websocket` | WebSocket |

### 5.2 의존성 구조도

```
                        istio.io/istio
                             │
         ┌───────────────────┼───────────────────┐
         │                   │                   │
    istio.io/api      istio.io/client-go    Envoy API
    (Istio 타입)       (K8s 클라이언트)   (go-control-plane)
         │                   │                   │
         └─────────┬─────────┘                   │
                   │                             │
              Protobuf/gRPC ◄────────────────────┘
              (google.golang.org/grpc)
                   │
         ┌─────────┼─────────┐
         │         │         │
    k8s.io/*   Prometheus   OpenTelemetry
   (K8s API)   (모니터링)    (트레이싱)
```

---

## 6. 테스트 구조

### 6.1 테스트 디렉토리 개요

```
tests/                              # 최상위 테스트 디렉토리
├── integration/                    # 통합 테스트 (실제 K8s 클러스터 필요)
│   ├── ambient/                    #   Ambient 모드 테스트
│   │   ├── cni/                    #     CNI 테스트
│   │   ├── cnirepair/              #     CNI 복구 테스트
│   │   ├── cniupgrade/             #     CNI 업그레이드 테스트
│   │   ├── crl/                    #     인증서 해지 목록 테스트
│   │   ├── untaint/                #     노드 Untaint 테스트
│   │   └── waypoint/               #     Waypoint 프록시 테스트
│   ├── helm/                       #   Helm 설치 테스트
│   │   └── upgrade/                #     업그레이드 테스트
│   ├── pilot/                      #   Pilot(istiod) 통합 테스트
│   │   ├── analysis/               #     설정 분석 테스트
│   │   ├── cni/                    #     CNI 연동 테스트
│   │   ├── common/                 #     공통 테스트 유틸리티
│   │   ├── forwardproxy/           #     포워드 프록시 테스트
│   │   ├── gie/                    #     Gateway Injection 테스트
│   │   ├── mcs/                    #     Multi-Cluster Service 테스트
│   │   ├── multiplecontrolplanes/  #     다중 컨트롤 플레인 테스트
│   │   ├── nftables/               #     nftables 테스트
│   │   ├── proxyconfig/            #     ProxyConfig 테스트
│   │   ├── revisions/              #     리비전 관리 테스트
│   │   └── resourcefilter/         #     리소스 필터 테스트
│   ├── security/                   #   보안 통합 테스트
│   │   ├── ca_custom_root/         #     커스텀 루트 CA 테스트
│   │   ├── cacert_rotation/        #     CA 인증서 회전 테스트
│   │   ├── external_ca/            #     외부 CA 연동 테스트
│   │   ├── file_mounted_certs/     #     파일 마운트 인증서 테스트
│   │   ├── filebased_tls_origination/ # 파일 기반 TLS 시작 테스트
│   │   ├── fuzz/                   #     보안 퍼즈 테스트
│   │   ├── https_jwt/              #     HTTPS JWT 테스트
│   │   ├── pqc/                    #     양자내성암호(PQC) 테스트
│   │   ├── remote_jwks/            #     원격 JWKS 테스트
│   │   ├── sds_ingress/            #     SDS Ingress 테스트
│   │   └── policy_attachment_only/ #     정책 첨부 전용 테스트
│   └── telemetry/                  #   텔레메트리 통합 테스트
│       ├── api/                    #     API 테스트
│       ├── policy/                 #     정책 테스트
│       └── tracing/                #     트레이싱 테스트
├── fuzz/                           # 퍼즈 테스트
│   ├── testdata/                   #   퍼즈 코퍼스
│   └── utils/                      #   퍼즈 유틸리티
├── binary/                         # 바이너리 크기/동작 테스트
├── common/                         # 공통 테스트 리소스
│   └── jwt/                        #   JWT 테스트 토큰
├── testdata/                       # 테스트 데이터
│   ├── certs/                      #   인증서
│   ├── config/                     #   설정 파일
│   ├── networking/                 #   네트워킹 설정
│   └── multicluster/               #   다중 클러스터 설정
└── util/                           # 테스트 유틸리티
    ├── leak/                       #   리소스 누수 탐지
    ├── pki/                        #   PKI 유틸리티
    └── sanitycheck/                #   기본 동작 확인
```

### 6.2 단위 테스트 패턴

Istio는 Go 표준 테스트 프레임워크(`testing` 패키지)를 사용하며, 각 패키지 내에 `*_test.go` 파일로 단위 테스트를 배치한다.

**테스트 파일 명명 규칙**:
```
{기능}.go       →  소스 코드
{기능}_test.go  →  단위 테스트
leak_test.go    →  고루틴 누수 테스트 (거의 모든 패키지에 존재)
fuzz_test.go    →  퍼즈 테스트
bench_test.go   →  벤치마크 테스트
```

**테스트 유틸리티 위치**:
```
pkg/test/                           # 테스트 프레임워크
├── framework/                      #   통합 테스트 프레임워크
│   ├── components/                 #   테스트 컴포넌트 (Namespace, Echo 등)
│   ├── resource/                   #   리소스 관리
│   ├── label/                      #   테스트 레이블링
│   └── integration/                #   통합 테스트 런타임
├── echo/                           #   Echo 테스트 서버
│   ├── cmd/client/                 #   Echo 클라이언트 바이너리
│   ├── cmd/server/                 #   Echo 서버 바이너리
│   ├── proto/                      #   Echo gRPC 프로토콜
│   ├── common/                     #   Echo 공통 로직
│   └── server/                     #   Echo 서버 구현
├── cert/                           #   인증서 생성 헬퍼
├── util/                           #   테스트 유틸리티
│   ├── assert/                     #     단언 헬퍼
│   ├── retry/                      #     재시도 헬퍼
│   ├── yml/                        #     YAML 파싱
│   └── tmpl/                       #     템플릿 처리
├── env/                            #   테스트 환경 변수
├── fakes/                          #   Fake 구현체
├── loadbalancersim/                #   로드 밸런서 시뮬레이터
├── kube/                           #   K8s 테스트 헬퍼
└── scopes/                         #   로그 스코프
```

### 6.3 통합 테스트 실행 구조

Istio의 통합 테스트는 `pkg/test/framework/`를 사용하는 독자적인 프레임워크를 가지고 있다.

```
통합 테스트 실행 흐름:

1. 테스트 프레임워크 초기화
   └── pkg/test/framework/integration/

2. 테스트 환경 프로비저닝
   ├── 실제 K8s 클러스터 연결
   ├── Istio 설치 (테스트 프로파일)
   └── 테스트 네임스페이스 생성

3. Echo 서버 배포
   ├── pkg/test/echo/cmd/server     # 테스트용 HTTP/gRPC 서버
   └── pkg/test/echo/cmd/client     # 테스트용 클라이언트

4. 테스트 시나리오 실행
   ├── 트래픽 전송 (Echo 클라이언트 -> Echo 서버)
   ├── 정책 적용 및 검증
   └── 설정 변경 및 반영 확인

5. 정리
   └── 리소스 삭제, 네임스페이스 정리
```

### 6.4 리소스 누수 테스트

Istio는 거의 모든 패키지에 `leak_test.go`를 포함한다. 이 파일은 고루틴 누수를 감지하는 `TestMain` 함수를 정의한다.

```go
// 전형적인 leak_test.go 패턴
func TestMain(m *testing.M) {
    // 테스트 전후로 고루틴 수를 비교하여 누수 감지
    leak.CheckMain(m)
}
```

이 패턴은 장기 실행 프로세스인 istiod와 pilot-agent에서 고루틴 누수가 발생하면 메모리 문제를 일으킬 수 있기 때문에 중요하다.

### 6.5 테스트 실행 명령

```bash
# 모든 단위 테스트 실행 (Race detector 포함)
make test

# 특정 패키지 테스트
go test ./pilot/pkg/xds/...

# 벤치마크 테스트
make benchtest

# 통합 테스트 (실제 K8s 클러스터 필요)
go test ./tests/integration/pilot/... -tags=integ

# 퍼즈 테스트
go test -fuzz=FuzzConfigValidation2 ./tests/fuzz/
```

---

## 7. 코드 구조 요약

### 7.1 컴포넌트별 바이너리 매핑

```
┌─────────────────────────────────────────────────────────────────┐
│                         istio.io/istio                          │
├─────────────────┬───────────────┬──────────────┬───────────────┤
│ pilot-discovery │  pilot-agent  │   istioctl   │   istio-cni   │
│ (istiod)        │  (사이드카)    │   (CLI)      │   (CNI)       │
├─────────────────┼───────────────┼──────────────┼───────────────┤
│ pilot/cmd/      │ pilot/cmd/    │ istioctl/    │ cni/cmd/      │
│   pilot-discovery│  pilot-agent │   cmd/       │   istio-cni   │
│                 │               │   istioctl   │   install-cni │
│ pilot/pkg/      │ pkg/          │              │               │
│   bootstrap/    │   istio-agent/│ istioctl/    │ cni/pkg/      │
│   xds/          │   security/   │   pkg/       │   plugin/     │
│   networking/   │               │              │   iptables/   │
│   serviceregistry│ security/    │ operator/    │   nodeagent/  │
│   model/        │   pkg/        │   pkg/       │               │
│   config/       │   nodeagent/  │              │               │
├─────────────────┴───────────────┴──────────────┴───────────────┤
│                      공통 라이브러리 (pkg/)                      │
│  config/ | model/ | kube/ | security/ | hbone/ | workloadapi/  │
│  test/ | log/ | monitoring/ | version/ | ...                    │
└─────────────────────────────────────────────────────────────────┘
```

### 7.2 코드 규모

| 디렉토리 | 주요 Go 파일 수 | 역할 |
|----------|----------------|------|
| `pilot/pkg/xds/` | 49 | xDS 디스커버리 서비스 |
| `pilot/pkg/networking/core/` | 41 | Envoy 설정 생성 |
| `pilot/pkg/bootstrap/` | 18 | 서버 부트스트랩 |
| `pilot/pkg/model/` | 20+ | 핵심 데이터 모델 |
| `pkg/config/` | 다수 (하위 패키지 포함) | 설정 모델/스키마/분석 |
| `pkg/kube/` | 다수 (하위 패키지 포함) | K8s 클라이언트/인젝션 |
| `security/pkg/` | 다수 | CA, SDS, 인증서 관리 |
| `istioctl/pkg/` | 다수 (30+ 서브패키지) | CLI 서브커맨드 |

### 7.3 설계 원칙 요약

1. **관심사 분리**: `pilot/`(컨트롤 플레인), `pkg/`(공통), `security/`(보안), `cni/`(네트워크) 등 기능별 명확한 분리
2. **집약 패턴(Aggregate)**: 서비스 레지스트리와 설정 소스 모두 `aggregate/` 패키지로 여러 백엔드를 통합
3. **빌드 태그 최적화**: `agent` 태그로 사이드카 바이너리의 불필요한 의존성 제거
4. **테스트 계층화**: 단위 테스트(패키지 내), 통합 테스트(`tests/integration/`), 퍼즈 테스트(`tests/fuzz/`) 분리
5. **코드 생성**: Protobuf, CRD, 스키마 등 자동 생성 코드를 적극 활용
6. **프로파일 기반 설치**: Helm 프로파일과 IstioOperator 프로파일로 다양한 배포 시나리오 지원
7. **Ambient 모드 확장**: `pkg/hbone/`, `pkg/workloadapi/`, `pkg/zdsapi/`, `cni/pkg/nodeagent/` 등으로 사이드카 없는 메시 지원
