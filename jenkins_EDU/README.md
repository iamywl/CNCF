# Jenkins EDU

> Jenkins 소스코드 심층 분석 교육 자료

## Jenkins란?

Jenkins는 Java 기반의 오픈소스 자동화 서버로, CI/CD(지속적 통합/지속적 배포) 파이프라인을 구축하고 실행한다.
단일 WAR 파일로 배포되며, 1,800개 이상의 플러그인 생태계를 통해 거의 모든 빌드/배포 시나리오를 지원한다.

## 핵심 특징

- **단일 WAR 아키텍처**: 내장 Jetty(Winstone)로 독립 실행, 서블릿 컨테이너 배포도 가능
- **플러그인 시스템**: ExtensionPoint 기반, 독립 ClassLoader, 핫 로드/언로드
- **Descriptor/Describable 패턴**: 설정 UI 자동 생성, 메타데이터 관리
- **Stapler 웹 프레임워크**: URL 경로를 Java 객체 트리에 매핑
- **XML 영속성**: XStream2 기반, 원자적 쓰기, 스키마 진화 지원
- **빌드 큐/Executor**: 다단계 큐 상태 머신, 노드별 Executor 스레드풀

## 소스코드

- 저장소: https://github.com/jenkinsci/jenkins
- 언어: Java
- 소스 경로: `jenkins/` (이 모노레포 내)

## 문서 목차

### 기본 문서 (01~06)

| # | 문서 | 내용 |
|---|------|------|
| 01 | [아키텍처](01-architecture.md) | WAR 구조, 부팅 시퀀스, Reactor 초기화, 서브시스템 관계 |
| 02 | [데이터 모델](02-data-model.md) | Job/Run 계층, Queue 아이템, Node/Computer/Executor, Action/Descriptor |
| 03 | [시퀀스 다이어그램](03-sequence-diagrams.md) | 빌드 스케줄링, 플러그인 로드, Stapler 라우팅 등 주요 흐름 |
| 04 | [코드 구조](04-code-structure.md) | 디렉토리 구조, 빌드 시스템(Maven), 모듈 관계 |
| 05 | [핵심 컴포넌트](05-core-components.md) | Jenkins 싱글톤, PluginManager, Queue, SecurityRealm 등 |
| 06 | [운영](06-operations.md) | 설치, 설정, 백업, 모니터링, 트러블슈팅 |

### 심화 문서 (07~18)

| # | 문서 | 내용 |
|---|------|------|
| 07 | [Jenkins 싱글톤](07-jenkins-singleton.md) | Jenkins 클래스 구조, 전역 상태 관리, 라이프사이클 |
| 08 | [빌드 큐](08-build-queue.md) | 4단계 큐 상태 머신, LoadBalancer, QueueSorter |
| 09 | [Executor 시스템](09-executor-system.md) | Node-Computer-Executor 계층, 작업 할당 |
| 10 | [플러그인 시스템](10-plugin-system.md) | ClassLoader 계층, ExtensionPoint, 의존성 해석 |
| 11 | [Descriptor 패턴](11-descriptor-pattern.md) | Describable/Descriptor, 설정 폼, 메타데이터 |
| 12 | [보안](12-security.md) | SecurityRealm, AuthorizationStrategy, ACL, CSRF |
| 13 | [Stapler 웹](13-stapler-web.md) | URL-to-Object 매핑, 액션 메서드, 뷰 렌더링 |
| 14 | [XML 영속성](14-xml-persistence.md) | XStream2, AtomicFileWriter, BulkChange |
| 15 | [초기화 시스템](15-init-system.md) | InitMilestone, Reactor, @Initializer |
| 16 | [CLI & Remoting](16-cli-remoting.md) | PlainCLIProtocol, CLICommand, Remoting Channel |
| 17 | [빌드 파이프라인](17-build-pipeline.md) | BuildStep, Builder, Publisher, BuildWrapper, Trigger |
| 18 | [SCM & View](18-scm-view.md) | SCM 폴링, ChangeLogSet, View 필터링, Fingerprint |

### PoC (Proof of Concept)

| # | PoC | 핵심 시뮬레이션 |
|---|-----|----------------|
| 01 | [아키텍처](poc-01-architecture/) | WAR 디스패처, InitMilestone DAG 위상정렬 |
| 02 | [데이터 모델](poc-02-data-model/) | Job/Run 계층 구조, 빌드 라이프사이클 |
| 03 | [빌드 큐](poc-03-build-queue/) | 4단계 큐 상태 머신, 로드밸런싱 |
| 04 | [Executor](poc-04-executor/) | Node-Computer-Executor 분리, 작업 할당 |
| 05 | [플러그인 시스템](poc-05-plugin-system/) | ClassLoader 계층, ExtensionPoint 레지스트리 |
| 06 | [Descriptor](poc-06-descriptor/) | Describable/Descriptor 패턴, 메타데이터 레지스트리 |
| 07 | [보안](poc-07-security/) | SecurityRealm/AuthorizationStrategy, ACL, CSRF |
| 08 | [Stapler 라우팅](poc-08-stapler-routing/) | URL-to-Object 매핑, 객체 그래프 탐색 |
| 09 | [XML 영속성](poc-09-xml-persistence/) | 원자적 파일 쓰기, XStream 직렬화/역직렬화 |
| 10 | [초기화 마일스톤](poc-10-init-milestone/) | Reactor DAG, 마일스톤 순서 보장, 병렬 실행 |
| 11 | [CLI](poc-11-cli/) | CLI 프로토콜 프레임, 명령 디스패치 |
| 12 | [빌드 파이프라인](poc-12-build-pipeline/) | BuildStep 체인, BuildWrapper, Trigger |
| 13 | [SCM 폴링](poc-13-scm-polling/) | SCM 변경 감지, PollingResult, ChangeLogSet |
| 14 | [View 시스템](poc-14-view-system/) | ListView/AllView, 필터링, 상태 집계 |
| 15 | [BulkChange](poc-15-bulk-change/) | 저장 트랜잭션, 배치 저장 최적화 |
| 16 | [Fingerprint](poc-16-fingerprint/) | MD5 기반 아티팩트 추적, RangeSet |

## 실행 방법

```bash
# 각 PoC 실행
cd jenkins_EDU/poc-01-architecture
go run main.go
```

모든 PoC는 Go 표준 라이브러리만 사용하며, 외부 의존성 없이 `go run main.go`로 실행 가능하다.
