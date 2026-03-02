# Application Definition & Image Build

앱을 정의하고, 컨테이너 이미지를 빌드하고, K8s에 배포하는 도구들.

---

## Graduated

### Helm ★29.6k
- **역할**: Kubernetes 패키지 매니저
- **핵심 개념**: Chart(패키지), Values(설정), Release(설치 인스턴스)
- **왜 쓰나**: 수십 개의 K8s YAML을 하나의 Chart로 묶어 `helm install` 한 줄로 설치/업그레이드/삭제
- **비유**: apt, brew 같은 것의 K8s 버전
- **예시**:
  ```bash
  helm install cilium cilium/cilium --values cilium-values.yaml
  helm upgrade cilium cilium/cilium --reuse-values --values hubble-values.yaml
  ```

### Dapr ★25.5k
- **역할**: 분산 앱 개발용 포터블 런타임
- **핵심 개념**: 사이드카 패턴으로 상태 관리, 서비스 호출, Pub/Sub, 바인딩 등을 추상화
- **왜 쓰나**: 마이크로서비스 개발 시 인프라 관심사(메시징, 상태 저장 등)를 앱 코드에서 분리
- **비유**: 마이크로서비스를 위한 만능 비서 — 메시지 전달, 상태 기억, 외부 연동을 다 대신해줌

---

## Incubating

### Backstage ★32.7k
- **역할**: 개발자 포탈 프레임워크 (Spotify 개발)
- **왜 쓰나**: 내부 서비스 카탈로그, API 문서, CI/CD 상태, 인프라 관리를 하나의 UI로 통합
- **비유**: 회사 내부의 "서비스 네이버" — 모든 내부 도구/서비스를 한 곳에서 검색/관리

### Buildpacks ★2.9k
- **역할**: 소스코드 → 컨테이너 이미지 자동 빌드
- **왜 쓰나**: Dockerfile 없이 소스만 넣으면 알아서 언어를 감지하고 이미지를 빌드
- **비유**: 소스코드를 넣으면 알아서 포장해주는 자동 포장기

### Artifact Hub ★2k
- **역할**: 클라우드 네이티브 패키지 검색/배포 허브
- **왜 쓰나**: Helm Chart, OPA 정책, Falco 룰 등을 한 곳에서 검색/설치
- **비유**: 클라우드 네이티브 앱스토어

### KubeVela ★7.7k
- **역할**: OAM(Open Application Model) 기반 앱 배포 플랫폼
- **왜 쓰나**: 개발자가 인프라 세부사항 없이 앱 배포를 선언적으로 정의
- **비유**: "이 앱은 3개 복제본에 오토스케일링 해줘"라고 말하면 알아서 K8s 리소스를 생성

### KubeVirt ★6.7k
- **역할**: Kubernetes 안에서 VM(가상 머신) 실행
- **동작 원리**: Pod 안에서 QEMU/KVM 프로세스를 실행하여 게스트 OS 부팅
- **제약**: Linux KVM 필수. Apple Silicon에서는 사용 불가 (KVM 없음, 중첩 가상화 미지원)
- **왜 쓰나**: 컨테이너화하기 어려운 레거시 워크로드를 K8s에서 통합 관리
- **구조**:
  ```
  Node (Linux)
  ├── kubelet
  │   └── Pod (virt-launcher)
  │       └── QEMU 프로세스 → VM (Windows, Linux 등)
  └── virt-handler (DaemonSet) — VM 라이프사이클 관리
  ```

### Operator Framework ★7.6k
- **역할**: Kubernetes Operator 개발 SDK
- **왜 쓰나**: CRD(Custom Resource Definition) + Controller 패턴으로 복잡한 앱의 Day-2 운영 자동화
- **비유**: K8s에게 "이 앱은 이렇게 운영해"라고 가르치는 교육 프레임워크

### OpenKruise ★5.2k
- **역할**: 대규모 K8s 앱 자동화 관리 (Alibaba)
- **왜 쓰나**: 기본 Deployment/StatefulSet보다 고급 롤아웃 전략, 사이드카 관리, 이미지 프리워밍 제공

---

## Sandbox (주요)

| 도구 | ★ | 설명 |
|---|---|---|
| **ko** | 8.4k | Go 앱을 K8s에 빠르게 빌드/배포 |
| **Score** | 8k | 환경 독립적 워크로드 스펙 정의 |
| **Podman Desktop** | 7.3k | 컨테이너/K8s 관리 데스크톱 UI |
| **Telepresence** | 7.1k | 로컬 개발 환경을 원격 K8s 클러스터에 연결 |
| **DevSpace** | 4.9k | K8s 개발 워크플로우 자동화 |
| **Microcks** | 1.8k | API 모킹 및 테스트 |
| **Carvel** | 1.8k | K8s 앱 빌드/배포용 경량 도구 모음 |
| **Radius** | 1.6k | 클라우드 네이티브 앱 플랫폼 (Microsoft) |
| **Porter** | 1.4k | 앱+도구+설정을 하나의 설치 패키지로 번들링 |
| **KUDO** | 1.2k | 선언적 K8s Operator 프레임워크 |
| **Konveyor** | 35 | 레거시 앱 → K8s 마이그레이션 분석/변환 |
| **Devfile** | 333 | 클라우드 개발 환경 스펙 정의 |
| **Dalec** | 276 | 선언적 시스템 패키지/컨테이너 빌드 |
| **ModelPack** | 180 | AI 아티팩트 패키징/배포 표준 |
| **Shipwright** | 806 | K8s에서 컨테이너 이미지 빌드 프레임워크 |
| **Stacker** | 319 | OCI 이미지 선언적 빌드 + SBOM(Software Bill of Materials) |
| **Serverless Workflow** | 877 | 서버리스 워크플로우 DSL(Domain Specific Language) 표준 |
| **VS Code K8s Tools** | 753 | VS Code Kubernetes 확장 |
| **xRegistry** | 12 | 리소스 메타데이터 관리 레지스트리 표준 |

---

## Archived

| 도구 | ★ | 설명 | 비고 |
|---|---|---|---|
| **Krator** | 168 | K8s Rust 상태 머신 Operator | 커뮤니티 비활성화 |
| **Nocalhost** | 1.9k | 클라우드 개발 환경 | DevSpace 등으로 대체 |
| **sealer** | 2.1k | K8s 클러스터+앱 통째로 패키징 | 커뮤니티 비활성화 |

---

## 주요 비(非)-CNCF 도구

| 도구 | ★ | 설명 |
|---|---|---|
| **Docker Compose** | 37.1k | 멀티 컨테이너 앱 정의/실행 |
| **Podman** | 30.8k | 데몬 없는 컨테이너 엔진 (Docker 대안) |
| **OpenAPI** | 30.9k | REST API 스펙 표준 |
| **Gradle** | 18.4k | 다국어 빌드 자동화 도구 (Java/Kotlin/C++) |
| **Packer** | 15.6k | 멀티플랫폼 머신 이미지 빌드 (HashiCorp) |
| **Skaffold** | 15.8k | K8s 개발 워크플로우 자동화 (Google) |
| **Quarkus** | 15.5k | K8s 네이티브 Java 프레임워크 (GraalVM 최적화) |
| **Coder** | 12.3k | 셀프호스트 클라우드 개발 환경 |
| **Daytona** | 61.1k | AI 코드 실행용 보안 인프라 |
| **Tilt** | 9.5k | 마이크로서비스 로컬 개발 환경 자동화 |
| **Eclipse Che** | 7.1k | K8s 기반 클라우드 IDE |
| **Maven** | 5k | Java 프로젝트 관리/빌드 도구 (Apache) |
| **kaniko** | 837 | K8s Pod에서 Docker 데몬 없이 이미지 빌드 (Google) |
| **Fabric8** | 3.6k | K8s Java 클라이언트 라이브러리 |
| **Tanka** | 2.6k | Jsonnet 기반 K8s 설정 관리 (Grafana Labs) |
| **Open Application Model** | 3.2k | 앱 배포 모델 표준 (KubeVela의 기반) |
