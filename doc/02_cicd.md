# Continuous Integration & Delivery

코드 변경을 자동으로 빌드, 테스트, 배포하는 도구들.

- **CI(Continuous Integration)**: 코드 변경 시 자동 빌드 + 테스트
- **CD(Continuous Delivery/Deployment)**: 테스트 통과한 코드를 자동으로 배포

---

## Graduated

### Argo ★22.1k
- **역할**: K8s 네이티브 워크플로우 + GitOps
- **구성 요소**:
  - **Argo CD**: GitOps 기반 K8s 배포 (Git 저장소 = 배포 상태의 진실 공급원)
  - **Argo Workflows**: K8s에서 DAG(Directed Acyclic Graph) 기반 워크플로우 실행
  - **Argo Rollouts**: 카나리/블루그린 점진적 배포
  - **Argo Events**: 이벤트 기반 트리거
- **왜 쓰나**: K8s 환경에서 GitOps + CI/CD + 고급 배포 전략을 통합 제공
- **비유**: K8s 전용 올인원 배포 관제탑

### Flux ★7.9k
- **역할**: GitOps 기반 K8s 지속적 배포
- **왜 쓰나**: Git 저장소를 감시하다가 변경이 생기면 자동으로 K8s에 적용
- **Argo CD와의 차이**: Flux는 CLI/API 중심 (UI 없음), 더 경량. Argo CD는 Web UI 제공, 기능이 더 풍부
- **비유**: Git에 코드를 올리면 알아서 배포해주는 자동 동기화 시스템

---

## Incubating

### OpenKruise ★5.2k
- **역할**: 대규모 K8s 앱 자동화 관리 (Alibaba)
- **왜 쓰나**: K8s 기본 Deployment보다 고급 롤아웃 전략 (In-place Update, 사이드카 관리, 이미지 프리워밍)

---

## Sandbox

| 도구 | ★ | 설명 |
|---|---|---|
| **OpenGitOps** | 1.1k | GitOps 원칙/표준 정의 프로젝트 |
| **PipeCD** | 1.3k | GitOps 스타일 CD 플랫폼 (다양한 프로바이더 지원) |
| **werf** | 4.7k | Git + Docker + Helm + K8s 통합 CI/CD 솔루션 |
| **Kube-burner** | 721 | K8s 성능/스케일 테스트 오케스트레이션 |
| **OpenChoreo** | 669 | K8s용 개발자 플랫폼 (CI/CD + 관측성 + Backstage 포탈) |

---

## Archived

| 도구 | ★ | 설명 | 비고 |
|---|---|---|---|
| **Brigade** | 2.4k | K8s 이벤트 기반 스크립팅 | 커뮤니티 비활성화 |
| **Keptn** | 407 | SLO(Service Level Objective) 기반 앱 라이프사이클 오케스트레이션 | 커뮤니티 비활성화 |

---

## 주요 비-CNCF 도구

### CI/CD 플랫폼

| 도구 | ★ | 설명 |
|---|---|---|
| **GitHub Actions** | - | GitHub 통합 CI/CD (YAML 기반 워크플로우) |
| **GitLab** | 24.2k | 올인원 DevOps 플랫폼 (소스 관리 + CI/CD + 레지스트리) |
| **Jenkins** | 25.1k | 가장 오래되고 널리 쓰이는 CI/CD 서버 (플러그인 생태계) |
| **Tekton** | 8.9k | K8s 네이티브 CI/CD 파이프라인 (Google, CDF 프로젝트) |
| **Gitness** | 33.8k | 오픈소스 소스 관리 + CI/CD (Harness) |
| **CircleCI** | - | 클라우드 CI/CD SaaS |
| **Concourse** | 7.8k | 컨테이너 기반 CI 시스템 (리소스 모델) |
| **GoCD** | 7.4k | CD 서버 (ThoughtWorks, 파이프라인 시각화 강점) |
| **JenkinsX** | 4.7k | K8s용 Jenkins (Tekton 기반) |
| **Buildkite** | - | 자체 인프라에서 돌리는 CI/CD (에이전트 기반) |
| **Woodpecker CI** | 6.5k | Drone CI 포크, 경량 CI/CD 엔진 |
| **Travis CI** | 611 | 오픈소스 프로젝트용 CI SaaS |
| **Spinnaker** | 9.7k | 멀티클라우드 CD 플랫폼 (Netflix) |
| **TeamCity** | - | JetBrains CI/CD 서버 |
| **Bamboo** | - | Atlassian CI/CD 서버 |

### 배포 전략/자동화

| 도구 | ★ | 설명 |
|---|---|---|
| **Flagger** | 5.3k | 카나리/A/B 테스트/블루그린 점진적 배포 Operator |
| **Helmwave** | 878 | Helm 릴리즈 선언적 관리 도구 |
| **Devtron** | 5.4k | K8s 오픈소스 소프트웨어 딜리버리 워크플로우 |

### 테스트/부하 테스트

| 도구 | ★ | 설명 |
|---|---|---|
| **k6** | 30k | 개발자 친화적 부하 테스트 도구 (Grafana Labs) |
| **Fortio** | 3.7k | 부하 테스트 라이브러리 (Istio 성능 측정용으로 시작) |
| **Testkube** | 1.6k | K8s 네이티브 테스트 프레임워크 |
| **Keploy** | 16.1k | API 통합/E2E 테스트 에이전트 (테스트 자동 생성) |

### IaC / 배포 관리

| 도구 | ★ | 설명 |
|---|---|---|
| **Terramate** | 3.5k | IaC(Infrastructure as Code) 관리 플랫폼 |
| **Liquibase** | 5.4k | DB 스키마 변경 관리 (DevOps for DB) |
| **Bytebase** | 13.8k | DB CI/CD 플랫폼 |

### GitOps

| 도구 | ★ | 설명 |
|---|---|---|
| **Codefresh** | - | Argo 기반 GitOps CI/CD 플랫폼 |
| **Razee** | 425 | K8s 배포 자동화 (IBM) |
