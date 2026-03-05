# Grafana 소스코드 교육 자료 (EDU)

## 프로젝트 개요

**Grafana**는 오픈소스 모니터링 및 옵저버빌리티 플랫폼이다. 메트릭, 로그, 트레이스 등 다양한 데이터 소스를 통합하여 대시보드로 시각화하고, 알림을 설정하며, 데이터를 탐색할 수 있다.

- **GitHub**: https://github.com/grafana/grafana
- **라이선스**: AGPL-3.0-only
- **언어**: Go (백엔드) + TypeScript/React (프론트엔드)
- **빌드**: Go 1.25 + Node.js 24 + Yarn 4.11

## 핵심 기능

| 기능 | 설명 |
|------|------|
| **시각화** | 30+ 패널 플러그인으로 메트릭/로그/트레이스 시각화 |
| **대시보드** | 동적 템플릿 변수, 반복 패널, 행 레이아웃 지원 |
| **데이터 소스** | Prometheus, Loki, Elasticsearch, InfluxDB 등 통합 |
| **알림** | 통합 알림 시스템 (Unified Alerting) with Alertmanager |
| **탐색** | Explore 모드 — 메트릭↔로그 자유 전환, 분할 화면 |
| **플러그인** | 백엔드(gRPC) + 프론트엔드(React) 플러그인 확장 |
| **RBAC** | 역할 기반 접근 제어, 조직(Org) 기반 멀티테넌시 |
| **프로비저닝** | YAML 파일로 대시보드/데이터소스/알림 자동 배포 |

## 아키텍처 한눈에 보기

```
┌─────────────────────────────────────────────────────┐
│                    Frontend (React/TS)               │
│  ┌──────────┐ ┌──────────┐ ┌───────────┐ ┌───────┐ │
│  │Dashboard │ │ Explore  │ │ Alerting  │ │Plugin │ │
│  │  Grid    │ │  Panes   │ │    UI     │ │ Admin │ │
│  └────┬─────┘ └────┬─────┘ └─────┬─────┘ └───┬───┘ │
│       └──────────┬──┴──────────┬──┘           │     │
│              Redux Store + RTK Query           │     │
└──────────────────┬─────────────────────────────┘     │
                   │ HTTP/WebSocket                     │
┌──────────────────┴───────────────────────────────────┐
│                  Backend (Go)                         │
│  ┌─────────┐  ┌──────────┐  ┌─────────┐  ┌────────┐│
│  │HTTP API │  │ Services │  │ Plugin  │  │ Alert  ││
│  │(web.Mux)│  │(Wire DI) │  │ Manager │  │ Engine ││
│  └────┬────┘  └────┬─────┘  └────┬────┘  └────┬───┘│
│       │        ┌───┴────┐        │             │    │
│       │        │SQLStore│   gRPC │        Scheduler │
│       │        │(XORM)  │        │             │    │
│  ┌────┴────┐   └───┬────┘  ┌────┴────┐   ┌────┴──┐ │
│  │Middleware│       │       │External │   │Alert  │ │
│  │ Chain   │    DB (SQLite/ │Plugins  │   │manager│ │
│  └─────────┘    Postgres/   └─────────┘   └───────┘ │
│                 MySQL)                               │
└──────────────────────────────────────────────────────┘
```

## EDU 문서 목차

### 기본 문서

| # | 문서 | 내용 |
|---|------|------|
| 01 | [아키텍처](01-architecture.md) | 전체 아키텍처, 컴포넌트 관계, 초기화 흐름 |
| 02 | [데이터 모델](02-data-model.md) | 핵심 데이터 구조, DB 스키마, K8s API |
| 03 | [시퀀스 다이어그램](03-sequence-diagrams.md) | 대시보드 로딩, 쿼리 실행, 알림 평가 흐름 |
| 04 | [코드 구조](04-code-structure.md) | 디렉토리 구조, 빌드 시스템, 의존성 |
| 05 | [핵심 컴포넌트](05-core-components.md) | HTTPServer, 서비스 레이어, 플러그인 매니저 |
| 06 | [운영 가이드](06-operations.md) | 배포, 설정, 모니터링, 트러블슈팅 |

### 심화 문서

| # | 문서 | 내용 |
|---|------|------|
| 07 | [백엔드 서버](07-backend-server.md) | Wire DI, HTTP 서버 라이프사이클, 백그라운드 서비스 |
| 08 | [대시보드 시스템](08-dashboard-system.md) | 대시보드 CRUD, 버전 관리, 프론트엔드 렌더링 |
| 09 | [데이터소스와 TSDB](09-datasource-tsdb.md) | 데이터 소스 프록시, 쿼리 백엔드, 미들웨어 스택 |
| 10 | [알림 시스템](10-alerting-system.md) | 스케줄러, 평가 엔진, 상태 머신, Alertmanager |
| 11 | [인증 시스템](11-authentication.md) | OAuth, JWT, LDAP, 세션, API 키 |
| 12 | [인가와 RBAC](12-authorization-rbac.md) | 접근 제어, 권한 평가, 스코프 리졸버 |
| 13 | [프론트엔드 아키텍처](13-frontend-architecture.md) | React/Redux, 라우팅, 패널 플러그인 SDK |
| 14 | [플러그인 시스템](14-plugin-system.md) | gRPC 프로토콜, 플러그인 라이프사이클, 미들웨어 |
| 15 | [프로비저닝](15-provisioning.md) | 파일 기반 설정, 대시보드/데이터소스/알림 프로비저닝 |
| 16 | [쿼리 실행 엔진](16-query-execution.md) | 쿼리 파이프라인, 서버사이드 표현식, DAG 실행 |
| 17 | [스토리지와 마이그레이션](17-storage-migration.md) | SQLStore, XORM, 통합 스토리지, DB 마이그레이션 |
| 18 | [옵저버빌리티](18-observability.md) | Prometheus 메트릭, OpenTelemetry 트레이싱, Live |

### PoC 코드

| # | PoC | 핵심 개념 |
|---|-----|----------|
| 01 | [Wire DI 시뮬레이션](poc-01-wire-di/) | 의존성 주입 컨테이너 |
| 02 | [대시보드 데이터 모델](poc-02-dashboard-model/) | Dashboard/Panel/DataSource 구조 |
| 03 | [쿼리 파이프라인](poc-03-query-pipeline/) | 쿼리 실행 → 데이터 변환 → 결과 반환 |
| 04 | [플러그인 라이프사이클](poc-04-plugin-system/) | 플러그인 등록/시작/통신/종료 |
| 05 | [알림 스케줄러](poc-05-alert-scheduler/) | 주기적 규칙 평가 |
| 06 | [알림 상태 머신](poc-06-state-machine/) | 상태 전이 (Normal↔Pending↔Alerting) |
| 07 | [미들웨어 체인](poc-07-middleware-chain/) | HTTP 미들웨어 체인 실행 |
| 08 | [RBAC 평가기](poc-08-rbac-evaluator/) | 권한 평가 (Action + Scope) |
| 09 | [설정 로딩](poc-09-config-loading/) | INI + 환경변수 + 명령줄 우선순위 |
| 10 | [라우트 레지스터](poc-10-route-register/) | 계층적 라우트 등록 |
| 11 | [데이터소스 프록시](poc-11-datasource-proxy/) | 리버스 프록시 |
| 12 | [프로비저닝 시스템](poc-12-provisioning/) | YAML 파일 기반 자동 배포 |
| 13 | [세션 인증](poc-13-session-auth/) | 토큰 발급/검증/갱신 |
| 14 | [피처 토글](poc-14-feature-toggles/) | 기능 플래그 시스템 |
| 15 | [이벤트 버스](poc-15-event-bus/) | 이벤트 발행/구독 패턴 |
| 16 | [그리드 레이아웃](poc-16-grid-layout/) | 대시보드 24컬럼 그리드 |
| 17 | [표현식 엔진](poc-17-expression-engine/) | DAG 기반 서버사이드 표현식 |
| 18 | [시계열 링 버퍼](poc-18-ring-buffer/) | 시계열 데이터 링 버퍼 |

## 소스코드 참조

본 교육 자료의 모든 소스코드 참조는 Grafana v12.x (2025년 기준) 소스코드에서 직접 확인한 것이다.

```
소스 위치: /Users/ywlee/sideproejct/CNCF/grafana/
모듈: github.com/grafana/grafana
Go: 1.25.7
Node.js: 24.x
```
