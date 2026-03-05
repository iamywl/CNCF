# Kubernetes 소스코드 교육 자료 (EDU)

## 프로젝트 개요

Kubernetes(K8s)는 컨테이너화된 애플리케이션의 배포, 확장, 관리를 자동화하는 오픈소스 컨테이너 오케스트레이션 플랫폼이다.
Google의 Borg 시스템에서 축적한 15년 이상의 대규모 프로덕션 운영 경험을 바탕으로 설계되었으며,
CNCF(Cloud Native Computing Foundation)에서 호스팅하는 졸업(Graduated) 프로젝트다.

- **소스코드**: Go 언어 (약 350만 줄)
- **모듈**: `k8s.io/kubernetes` (Go 1.25+)
- **라이선스**: Apache License 2.0
- **GitHub**: https://github.com/kubernetes/kubernetes

## 핵심 기능

| 기능 | 설명 |
|------|------|
| 컨테이너 오케스트레이션 | 여러 노드에 걸쳐 컨테이너 배포·관리 |
| 자동 스케줄링 | 리소스 요구사항에 따라 Pod를 최적 노드에 배치 |
| 셀프 힐링 | 장애 컨테이너 자동 재시작, 노드 장애 시 Pod 재배치 |
| 수평 확장 | HPA로 부하에 따른 자동 스케일링 |
| 서비스 디스커버리 | DNS 기반 서비스 발견 및 로드밸런싱 |
| 롤링 업데이트 | 무중단 배포 및 자동 롤백 |
| 선언적 구성 | YAML/JSON으로 원하는 상태(desired state)를 선언 |
| RBAC | 역할 기반 접근 제어로 세밀한 권한 관리 |

## 문서 목차

### 기본 문서 (01~06)

| # | 문서 | 내용 |
|---|------|------|
| 01 | [아키텍처](01-architecture.md) | 전체 아키텍처, 컴포넌트 관계, 초기화 흐름 |
| 02 | [데이터 모델](02-data-model.md) | 핵심 API 객체 (Pod, Service, Node), TypeMeta/ObjectMeta |
| 03 | [시퀀스 다이어그램](03-sequence-diagrams.md) | Pod 생성, 스케줄링, 컨트롤러 동작 흐름 |
| 04 | [코드 구조](04-code-structure.md) | 디렉토리 구조, 빌드 시스템, staging 구조 |
| 05 | [핵심 컴포넌트](05-core-components.md) | API Server, Scheduler, Controller Manager, Kubelet |
| 06 | [운영](06-operations.md) | 배포, 설정, 모니터링, 트러블슈팅 |

### 심화 문서 (07~26)

| # | 문서 | 내용 |
|---|------|------|
| 07 | [API Server](07-api-server.md) | 요청 처리, delegation chain, handler chain |
| 08 | [etcd 스토리지](08-etcd-storage.md) | etcd3 백엔드, watch, 암호화 |
| 09 | [스케줄러](09-scheduler.md) | 스케줄링 프레임워크, 플러그인, 확장 |
| 10 | [컨트롤러 매니저](10-controller-manager.md) | 컨트롤러 패턴, 리더 선출, 컨트롤 루프 |
| 11 | [Kubelet](11-kubelet.md) | Pod 라이프사이클, PLEG, CRI |
| 12 | [네트워킹](12-networking.md) | kube-proxy, Service, CNI |
| 13 | [인증·인가](13-auth-rbac.md) | 인증 체인, RBAC, 권한 검사 |
| 14 | [어드미션 컨트롤](14-admission.md) | 어드미션 플러그인, 웹훅, 정책 |
| 15 | [client-go](15-client-go.md) | Informer, DeltaFIFO, WorkQueue |
| 16 | [볼륨·스토리지](16-volume-storage.md) | Volume 플러그인, CSI, PV/PVC |
| 17 | [CRD·API 확장](17-crd-extensions.md) | CustomResourceDefinition, API Aggregation |
| 18 | [빌드·코드 생성](18-build-codegen.md) | Makefile, code-generator, staging 구조 |
| 19 | [Pod 라이프사이클·QoS](19-pod-lifecycle.md) | QoS 클래스, OOM Score, Eviction, Preemption, Priority |
| 20 | [워크로드 컨트롤러](20-workload-controllers.md) | StatefulSet, DaemonSet, Job, CronJob |
| 21 | [Service·Ingress](21-service-ingress.md) | ClusterIP/NodePort/LB, EndpointSlice, Ingress 라우팅 |
| 22 | [리소스 관리](22-resource-management.md) | LimitRange, ResourceQuota, Pod 평가자 |
| 23 | [API Machinery](23-api-machinery.md) | Scheme, 버전 변환, Strategic Merge Patch, SSA |
| 24 | [Cloud Provider](24-cloud-provider.md) | CCM, Cloud Provider Interface, Node/Service/Route 컨트롤러 |
| 25 | [kubectl](25-kubectl.md) | Cobra, Factory, Apply(CSA/SSA), Printer, Plugin |
| 26 | [DNS·서비스 디스커버리](26-dns-discovery.md) | DNS Policy, resolv.conf, Service DNS, 환경변수 SD |

### PoC (Proof of Concept)

| # | PoC | 핵심 개념 |
|---|-----|----------|
| 01 | [아키텍처](poc-01-architecture/) | 컴포넌트 간 통신 패턴 |
| 02 | [데이터 모델](poc-02-data-model/) | TypeMeta/ObjectMeta/Spec/Status 패턴 |
| 03 | [API 요청 흐름](poc-03-api-request/) | Handler chain, REST 라우팅 |
| 04 | [스케줄러](poc-04-scheduler/) | Filter/Score 프레임워크 |
| 05 | [컨트롤러](poc-05-controller/) | Informer + WorkQueue 컨트롤 루프 |
| 06 | [Kubelet](poc-06-kubelet/) | Sync loop, PLEG |
| 07 | [etcd Watch](poc-07-etcd-watch/) | Watch + ResourceVersion |
| 08 | [RBAC](poc-08-rbac/) | 역할 기반 접근 제어 |
| 09 | [어드미션](poc-09-admission/) | 어드미션 체인 시뮬레이션 |
| 10 | [DeltaFIFO](poc-10-delta-fifo/) | Delta 누적 + FIFO 큐 |
| 11 | [WorkQueue](poc-11-workqueue/) | Rate-limiting 작업 큐 |
| 12 | [리더 선출](poc-12-leader-election/) | 리스 기반 리더 선출 |
| 13 | [서비스 디스커버리](poc-13-service-discovery/) | DNS + 엔드포인트 관리 |
| 14 | [HPA](poc-14-hpa/) | 수평 Pod 자동 확장 |
| 15 | [가비지 컬렉션](poc-15-garbage-collection/) | 소유자 참조 기반 GC |
| 16 | [볼륨 관리](poc-16-volume-manager/) | PV/PVC 바인딩, CSI |
| 17 | [CRD](poc-17-crd/) | 커스텀 리소스 정의·처리 |
| 18 | [코드 생성](poc-18-codegen/) | deepcopy, informer 생성 |
| 19 | [Pod 라이프사이클](poc-19-pod-lifecycle/) | QoS 계산, OOM Score, Eviction, Preemption |
| 20 | [워크로드 컨트롤러](poc-20-workload-controllers/) | StatefulSet, DaemonSet, Job, CronJob |
| 21 | [Service·Ingress](poc-21-service-ingress/) | IP/Port 할당, EndpointSlice, Ingress |
| 22 | [리소스 관리](poc-22-resource-management/) | LimitRange, ResourceQuota 어드미션 |
| 23 | [API Machinery](poc-23-api-machinery/) | Scheme, 변환, SMP, FieldManager |
| 24 | [Cloud Provider](poc-24-cloud-provider/) | CCM, Node/Service/Route 컨트롤러 |
| 25 | [kubectl](poc-25-kubectl/) | Cobra, Factory, Apply, Printer, Plugin |
| 26 | [DNS 디스커버리](poc-26-dns-discovery/) | DNS Policy, Service DNS, 환경변수 SD |

## 소스코드 참조

이 교육 자료의 모든 코드 참조는 Kubernetes 소스코드에서 직접 확인한 것이다.
추측으로 작성된 파일 경로나 함수명은 포함하지 않는다.

## 실행 환경

- Go 1.25 이상
- PoC 코드는 모두 `go run main.go`로 실행 가능
- 외부 의존성 없음 (Go 표준 라이브러리만 사용)
