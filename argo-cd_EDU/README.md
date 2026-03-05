# Argo CD 소스코드 교육 자료 (EDU)

## 프로젝트 개요

Argo CD는 Kubernetes를 위한 선언적(Declarative) GitOps 지속적 배포(Continuous Delivery) 도구다.
Git 저장소에 정의된 원하는 상태(desired state)와 클러스터의 실제 상태(live state)를 지속적으로 비교하여
자동으로 동기화하는 것이 핵심 기능이다.

- **소스코드**: Go 언어 (약 50만 줄)
- **모듈**: `github.com/argoproj/argo-cd/v3`
- **라이선스**: Apache License 2.0
- **GitHub**: https://github.com/argoproj/argo-cd

## 핵심 기능

| 기능 | 설명 |
|------|------|
| GitOps CD | Git 저장소를 Single Source of Truth로 사용하는 배포 자동화 |
| 선언적 관리 | Application CRD로 배포 대상·소스·정책을 선언 |
| 자동 동기화 | Git 변경 감지 → 자동 sync (autoSync + selfHeal) |
| 멀티소스 | Helm, Kustomize, 디렉토리, Plugin, OCI 지원 |
| ApplicationSet | 하나의 선언으로 다수 Application 자동 생성 |
| RBAC | Casbin 기반 역할 접근 제어 + 프로젝트 단위 격리 |
| SSO/OIDC | Dex/외부 OIDC 통합 인증 |
| 헬스 평가 | 리소스별 health 체크 (내장 + Lua 커스텀) |
| 알림 | Slack/Email/Webhook 등 다채널 알림 |
| 클러스터 샤딩 | 대규모 환경에서 컨트롤러 수평 확장 |

## 문서 목차

### 기본 문서 (01~06)

| # | 문서 | 내용 |
|---|------|------|
| 01 | [아키텍처](01-architecture.md) | 전체 아키텍처, 단일 바이너리 디스패처, cmux, 컴포넌트 관계 |
| 02 | [데이터 모델](02-data-model.md) | Application/AppProject/ApplicationSet CRD, 상태 전이 |
| 03 | [시퀀스 다이어그램](03-sequence-diagrams.md) | App 생성, Sync, Reconciliation, 인증 흐름 |
| 04 | [코드 구조](04-code-structure.md) | 디렉토리 구조, 빌드 시스템, 의존성 |
| 05 | [핵심 컴포넌트](05-core-components.md) | API Server, Application Controller, Repo Server |
| 06 | [운영](06-operations.md) | 배포, 설정, 모니터링, 트러블슈팅 |

### 심화 문서 (07~18)

| # | 문서 | 내용 |
|---|------|------|
| 07 | [API Server](07-api-server.md) | gRPC/HTTP 멀티플렉싱, 인증 체인, RBAC 미들웨어 |
| 08 | [Application Controller](08-application-controller.md) | Reconciliation 루프, CompareWith, autoSync, 작업 큐 |
| 09 | [Repo Server](09-repo-server.md) | 매니페스트 생성, 더블체크 락킹, 캐시, 세마포어 |
| 10 | [GitOps Engine](10-gitops-engine.md) | diff/sync/health/cache 4대 패키지, 분리 라이브러리 |
| 11 | [ApplicationSet](11-applicationset.md) | 9가지 Generator, Matrix/Merge, Reconcile 루프 |
| 12 | [RBAC·인증](12-rbac-auth.md) | Casbin 모델, enforce() 알고리즘, 세션 관리, 보안 패턴 |
| 13 | [Git 통합](13-git-integration.md) | nativeGitClient, 자격증명, repositoryLock |
| 14 | [Helm·Kustomize](14-helm-kustomize.md) | CLI 래핑, KeyLock, 마커 파일, OCI |
| 15 | [Sync Waves·Hooks](15-sync-waves-hooks.md) | Phase/Wave 상태 머신, Hook 라이프사이클 |
| 16 | [알림](16-notifications.md) | 트리거/템플릿/서비스, 어노테이션 구독, 중복 방지 |
| 17 | [캐싱·성능](17-caching-performance.md) | 3계층 캐시, 샤딩, 세마포어, 메트릭 |
| 18 | [설정 관리](18-settings-config.md) | ConfigMap/Secret 저장, FNV-32a 해시, 구독 패턴 |

### PoC (Proof of Concept)

| # | PoC | 핵심 개념 |
|---|-----|----------|
| 01 | [아키텍처](poc-01-architecture/) | 단일 바이너리 디스패처, cmux, 컴포넌트 간 통신 |
| 02 | [데이터 모델](poc-02-data-model/) | Application CRD 생명주기, 상태 전이 |
| 03 | [Reconciliation](poc-03-reconciliation/) | CompareWith, autoSync 가드, selfHeal 백오프 |
| 04 | [매니페스트 생성](poc-04-manifest-generation/) | 소스 타입 감지, 더블체크 락킹, 세마포어 |
| 05 | [GitOps Diff](poc-05-gitops-diff/) | ThreeWayDiff, TwoWayDiff, Strategic Merge Patch |
| 06 | [Sync Engine](poc-06-sync-engine/) | Phase/Wave 상태 머신, Prune 역순, Hook |
| 07 | [헬스 평가](poc-07-health-assessment/) | IsWorse, 리소스별 헬스 체크, Override |
| 08 | [RBAC](poc-08-rbac/) | Casbin 모델, glob 매칭, 프로젝트 정책 |
| 09 | [ApplicationSet](poc-09-applicationset/) | Generator 5종, Matrix/Merge, SyncPolicy |
| 10 | [Git 클라이언트](poc-10-git-client/) | Creds, RandomizedTempPaths, repositoryLock |
| 11 | [캐시](poc-11-cache/) | 3계층 캐시, PauseGeneration, CacheEntryHash |
| 12 | [설정 관리](poc-12-settings/) | FNV-32a, Subscribe/Notify, TrackingMethod |
| 13 | [알림](poc-13-notifications/) | 트리거/템플릿/서비스, SHA-256 중복 방지 |
| 14 | [Helm 템플릿](poc-14-helm-template/) | Values 병합, 마커 파일, pathLock |
| 15 | [세션](poc-15-session/) | HS256 JWT, 타이밍 노이즈, 실패 카운터 |
| 16 | [샤딩](poc-16-sharding/) | FNV-32a/RoundRobin/ConsistentHash, 하트비트 |

## 소스코드 참조

이 교육 자료의 모든 코드 참조는 Argo CD 소스코드에서 직접 확인한 것이다.
추측으로 작성된 파일 경로나 함수명은 포함하지 않는다.

## 실행 환경

- Go 1.25 이상
- PoC 코드는 모두 `go run main.go`로 실행 가능
- 외부 의존성 없음 (Go 표준 라이브러리만 사용)
