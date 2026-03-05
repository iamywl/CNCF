# Helm 교육 자료 (EDU)

> Kubernetes 패키지 매니저 — Chart를 통해 애플리케이션을 패키징, 배포, 관리

## 프로젝트 개요

Helm은 Kubernetes 애플리케이션을 **Chart**라는 패키지로 정의하고, 이를 클러스터에 설치·업그레이드·롤백하는 CLI 도구입니다.
apt/yum/homebrew의 Kubernetes 버전이라 할 수 있습니다.

### 핵심 특징

- **Chart 패키지**: `Chart.yaml` + Go 템플릿 + 기본값으로 구성된 재사용 가능한 패키지
- **Release 관리**: 설치된 Chart 인스턴스의 버전 추적, 롤백, 히스토리
- **템플릿 엔진**: Go text/template + Sprig 함수로 Kubernetes 매니페스트 동적 생성
- **4가지 스토리지 드라이버**: Secret(기본), ConfigMap, Memory, SQL(PostgreSQL)
- **OCI 레지스트리 지원**: Docker/GHCR 등 OCI 호환 레지스트리에 Chart Push/Pull
- **플러그인 시스템**: 외부 바이너리 및 WASM 기반 확장
- **Server-Side Apply**: Kubernetes SSA를 활용한 충돌 감지

### 기술 스택

| 항목 | 기술 |
|------|------|
| 언어 | Go 1.25 |
| CLI | spf13/cobra + spf13/pflag |
| 템플릿 | Go text/template + Masterminds/sprig |
| 스토리지 | Kubernetes Secret/ConfigMap, Memory, PostgreSQL |
| 레지스트리 | OCI (oras-go) |
| K8s 클라이언트 | client-go, cli-runtime |
| 차트 포맷 | Chart v2 (Helm v3/v4) |
| 값 병합 | YAML 기반 deep merge |

## 문서 목차

### 기본 문서

| # | 문서 | 내용 |
|---|------|------|
| 01 | [아키텍처](01-architecture.md) | 전체 아키텍처, 계층 구조, 초기화 흐름 |
| 02 | [데이터 모델](02-data-model.md) | Chart, Release, Values, Hook, Metadata 등 핵심 구조체 |
| 03 | [시퀀스 다이어그램](03-sequence-diagrams.md) | Install, Upgrade, Rollback, Pull/Push 주요 흐름 |
| 04 | [코드 구조](04-code-structure.md) | 디렉토리 구조, 패키지 의존성, 빌드 시스템 |
| 05 | [핵심 컴포넌트](05-core-components.md) | Configuration, Engine, Storage, KubeClient 동작 원리 |
| 06 | [운영 가이드](06-operations.md) | 설치, 설정, 환경변수, 트러블슈팅 |

### 심화 문서

| # | 문서 | 내용 |
|---|------|------|
| 07 | [Chart 시스템](07-chart-system.md) | Chart v2 구조, Metadata, Dependency, Loader |
| 08 | [템플릿 엔진](08-template-engine.md) | Engine, Sprig 함수, include/tpl, 렌더링 파이프라인 |
| 09 | [Release 라이프사이클](09-release-lifecycle.md) | Release 상태 머신, Info, Status 전이 |
| 10 | [스토리지 드라이버](10-storage-drivers.md) | Driver 인터페이스, Secret/ConfigMap/Memory/SQL 구현 |
| 11 | [Kubernetes 클라이언트](11-kubernetes-client.md) | kube.Interface, Client, Waiter, SSA/CSA |
| 12 | [OCI 레지스트리](12-oci-registry.md) | OCI 프로토콜, Push/Pull, 인증, 캐시 |
| 13 | [CLI 커맨드](13-cli-commands.md) | Cobra 구조, 30+ 서브커맨드, 셸 자동완성 |
| 14 | [Action 시스템](14-action-system.md) | Configuration, Install/Upgrade/Rollback/Uninstall 액션 |
| 15 | [의존성 관리](15-dependency-resolver.md) | Dependency 해석, Lock 파일, Downloader |
| 16 | [리포지토리 시스템](16-repository-system.md) | Chart 리포지토리, IndexFile, Search |
| 17 | [Values와 렌더링](17-values-rendering.md) | Values 병합, Schema 검증, PostRenderer |
| 18 | [플러그인 시스템](18-plugin-system.md) | 플러그인 구조, 설치, WASM 지원 |

### PoC (Proof of Concept)

| # | PoC | 핵심 시뮬레이션 |
|---|-----|----------------|
| 01 | [아키텍처](poc-01-architecture/) | CLI 디스패치, Action 패턴, Configuration 주입 |
| 02 | [데이터 모델](poc-02-data-model/) | Chart, Release, Values, Hook 구조체 |
| 03 | [템플릿 엔진](poc-03-template-engine/) | Go 템플릿 + 커스텀 함수로 매니페스트 렌더링 |
| 04 | [Chart 로더](poc-04-chart-loader/) | Chart.yaml 파싱, 의존성 트리 구성 |
| 05 | [스토리지 드라이버](poc-05-storage-driver/) | Driver 인터페이스와 Memory/File 구현 |
| 06 | [Release 라이프사이클](poc-06-release-lifecycle/) | Install→Upgrade→Rollback 상태 전이 |
| 07 | [Kubernetes 클라이언트](poc-07-kubernetes-client/) | Resource CRUD, Wait/Watch 시뮬레이션 |
| 08 | [OCI 레지스트리](poc-08-oci-registry/) | OCI Push/Pull 프로토콜 시뮬레이션 |
| 09 | [Hook 시스템](poc-09-hook-system/) | Pre/Post 훅, Weight 정렬, 삭제 정책 |
| 10 | [Values 병합](poc-10-values-merge/) | Deep merge, 스키마 검증 |
| 11 | [의존성 해석](poc-11-dependency-resolver/) | SemVer 제약 조건 기반 해석 |
| 12 | [리포지토리 인덱스](poc-12-repository-index/) | IndexFile 관리, Chart 검색 |
| 13 | [PostRenderer](poc-13-post-renderer/) | 후처리 파이프라인 시뮬레이션 |
| 14 | [플러그인 시스템](poc-14-plugin-system/) | 플러그인 탐색, 로드, 실행 |
| 15 | [Getter 체인](poc-15-getter-chain/) | URL 스킴 기반 다운로더 선택 |
| 16 | [Dry Run](poc-16-dry-run/) | Client/Server dry run 시뮬레이션 |

## 소스코드 참조

- 소스 경로: `/Users/ywlee/sideproejct/CNCF/helm/`
- GitHub: https://github.com/helm/helm
- 언어: Go 1.25
- 모듈: `helm.sh/helm/v4`
- 현재 버전: v4 (개발 중, main 브랜치)
