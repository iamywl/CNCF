# Prometheus 교육 자료 (EDU)

## 프로젝트 개요

Prometheus는 CNCF(Cloud Native Computing Foundation) 졸업 프로젝트로, 시스템과 서비스를 모니터링하는 오픈소스 시계열 데이터베이스이자 알림 시스템이다. 설정된 타겟으로부터 메트릭을 Pull 방식으로 수집하고, 규칙 표현식을 평가하며, 지정 조건이 관찰되면 알림을 발생시킨다.

### 핵심 특징

| 특징 | 설명 |
|------|------|
| 다차원 데이터 모델 | 메트릭 이름 + 키/값 레이블 차원으로 시계열 정의 |
| PromQL | 다차원 데이터를 활용하는 강력한 쿼리 언어 |
| 독립적 저장소 | 분산 스토리지 의존성 없음, 단일 서버 노드 자율 운영 |
| Pull 모델 | HTTP를 통한 시계열 수집 (타겟의 /metrics 엔드포인트 스크래핑) |
| Push 지원 | 배치 작업을 위한 Pushgateway 중간 게이트웨이 |
| 서비스 디스커버리 | Kubernetes, Consul, DNS 등 다양한 SD 메커니즘 |
| Federation | 계층적/수평적 연합 지원으로 대규모 환경 모니터링 |

### 아키텍처 한눈에 보기

```
┌──────────────────────────────────────────────────────────────────────┐
│                        Prometheus Server                              │
│                                                                       │
│  ┌─────────────┐    ┌─────────────┐    ┌──────────────┐              │
│  │  Service     │───→│   Scrape    │───→│    TSDB      │              │
│  │  Discovery   │    │   Manager   │    │  (Storage)   │              │
│  └─────────────┘    └─────────────┘    └──────┬───────┘              │
│                                                │                      │
│  ┌─────────────┐    ┌─────────────┐           │                      │
│  │   Rule      │←───│   PromQL    │←──────────┘                      │
│  │   Manager   │    │   Engine    │                                   │
│  └──────┬──────┘    └─────────────┘                                   │
│         │                                                             │
│  ┌──────▼──────┐    ┌─────────────┐    ┌──────────────┐              │
│  │  Notifier   │───→│ Alertmanager│    │  Web UI/API  │              │
│  └─────────────┘    └─────────────┘    └──────────────┘              │
└──────────────────────────────────────────────────────────────────────┘
```

## 소스코드 정보

- **언어**: Go
- **저장소**: https://github.com/prometheus/prometheus
- **라이선스**: Apache License 2.0
- **소스 위치**: `/prometheus/` (이 모노레포 내)

## 교육 자료 목차

### 기본 문서

| 번호 | 문서 | 내용 |
|------|------|------|
| 01 | [아키텍처](01-architecture.md) | 전체 아키텍처, 컴포넌트 관계, 초기화 흐름 |
| 02 | [데이터 모델](02-data-model.md) | 시계열 데이터 모델, TSDB 구조, 핵심 타입 |
| 03 | [시퀀스 다이어그램](03-sequence-diagrams.md) | 스크래핑, 쿼리, 알림 흐름 |
| 04 | [코드 구조](04-code-structure.md) | 디렉토리 구조, 빌드 시스템, 의존성 |
| 05 | [핵심 컴포넌트](05-core-components.md) | TSDB, PromQL, Scrape Manager 동작 원리 |
| 06 | [운영 가이드](06-operations.md) | 배포, 설정, 모니터링, 트러블슈팅 |

### 심화 문서

| 번호 | 문서 | 내용 |
|------|------|------|
| 07 | [TSDB Head & WAL](07-tsdb-head-wal.md) | Head 블록, WAL, 인메모리 시계열 관리 |
| 08 | [TSDB 블록 & 압축](08-tsdb-block-compaction.md) | 블록 라이프사이클, 레벨 기반 압축 |
| 09 | [TSDB 인덱스 & 포스팅](09-tsdb-index-postings.md) | 역색인, Symbol Table, 쿼리 최적화 |
| 10 | [Chunk 인코딩](10-chunk-encoding.md) | XOR 압축, 히스토그램 인코딩 |
| 11 | [스크래프 엔진](11-scrape-engine.md) | scrapePool, scrapeLoop, 메트릭 파싱 |
| 12 | [서비스 디스커버리](12-service-discovery.md) | SD 프레임워크, Kubernetes SD, 타겟 관리 |
| 13 | [PromQL 엔진](13-promql-engine.md) | 파싱, AST, 평가 파이프라인 |
| 14 | [규칙 엔진](14-rule-engine.md) | Recording Rule, Alerting Rule, 그룹 평가 |
| 15 | [알림 파이프라인](15-alerting-pipeline.md) | Alert 생명주기, Notifier, Alertmanager 연동 |
| 16 | [원격 저장소](16-remote-storage.md) | Remote Write/Read, 팬아웃 패턴 |
| 17 | [HTTP API & Web](17-http-api-web.md) | REST API, Web UI, 미들웨어 |
| 18 | [Relabeling & 설정](18-relabeling-config.md) | 설정 파이프라인, Relabel 규칙, 동적 설정 |

### PoC (Proof of Concept)

| PoC | 주제 | 핵심 개념 |
|-----|------|----------|
| poc-01 | [시계열 저장소](poc-01-timeseries-store/) | 인메모리 시계열 + 레이블 인덱스 |
| poc-02 | [XOR 압축](poc-02-xor-encoding/) | Delta-of-Delta + XOR 부동소수점 압축 |
| poc-03 | [WAL 구현](poc-03-wal/) | Write-Ahead Log + 재생 복구 |
| poc-04 | [블록 압축](poc-04-block-compaction/) | 2시간 블록 + 레벨 기반 머지 |
| poc-05 | [역색인](poc-05-inverted-index/) | 레이블 → 시계열 ID 역색인 |
| poc-06 | [스크래프 루프](poc-06-scrape-loop/) | HTTP Pull + 메트릭 파싱 + 저장 |
| poc-07 | [서비스 디스커버리](poc-07-service-discovery/) | 플러거블 SD 프레임워크 |
| poc-08 | [PromQL 평가](poc-08-promql-evaluator/) | AST 파싱 + 벡터/매트릭스 평가 |
| poc-09 | [Relabeling](poc-09-relabeling/) | 레이블 변환 파이프라인 |
| poc-10 | [알림 규칙](poc-10-alerting-rules/) | 알림 상태머신 + 발송 로직 |
| poc-11 | [Recording Rule](poc-11-recording-rules/) | 사전 계산 + 결과 저장 |
| poc-12 | [원격 쓰기](poc-12-remote-write/) | 배치 큐 + HTTP 전송 |
| poc-13 | [팬아웃 저장소](poc-13-fanout-storage/) | 로컬/원격 동시 읽기·쓰기 |
| poc-14 | [HTTP API 서버](poc-14-http-api/) | REST API + PromQL 쿼리 엔드포인트 |
| poc-15 | [설정 리로드](poc-15-config-reload/) | SIGHUP + 체크섬 기반 자동 리로드 |
| poc-16 | [Stripe 해시맵](poc-16-stripe-hashmap/) | 분산 잠금 동시성 자료구조 |
