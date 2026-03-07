# Prometheus Alertmanager 교육 자료

## 프로젝트 개요

Prometheus Alertmanager는 Prometheus 서버 등 클라이언트 애플리케이션이 보낸 알림(Alert)을 처리하는 핵심 컴포넌트이다. 알림의 **중복 제거(Deduplication)**, **그룹핑(Grouping)**, **라우팅(Routing)**을 담당하며, Email, PagerDuty, Slack, Webhook 등 다양한 수신자(Receiver)에게 알림을 전달한다. 또한 **Silence**(침묵)와 **Inhibition**(억제)을 통해 알림을 제어할 수 있다.

- **언어**: Go
- **라이선스**: Apache License 2.0
- **GitHub**: https://github.com/prometheus/alertmanager
- **공식 문서**: https://prometheus.io/docs/alerting/alertmanager/

## 핵심 기능

| 기능 | 설명 |
|------|------|
| 중복 제거 | 동일한 알림을 하나로 합침 |
| 그룹핑 | 관련 알림을 레이블 기준으로 묶어서 한 번에 전송 |
| 라우팅 | 트리 구조의 라우팅 규칙으로 적절한 수신자에게 전달 |
| Silence | 특정 레이블 매칭 조건으로 알림을 일시적으로 억제 |
| Inhibition | 특정 알림이 발생하면 관련 알림을 자동 억제 |
| HA 클러스터링 | Gossip 프로토콜로 다중 인스턴스 간 상태 동기화 |
| 템플릿 | Go 템플릿으로 알림 메시지 커스터마이징 |
| REST API v2 | OpenAPI 기반 API로 알림, Silence 관리 |

## 아키텍처 요약

```
Prometheus / 클라이언트
        │
        ▼
┌─── Alertmanager ───────────────────────────────┐
│                                                 │
│  [API v2] ──→ [Provider(메모리)] ──→ [Dispatcher]│
│                                      │          │
│                              ┌───────┴───────┐  │
│                              │ Route 트리    │  │
│                              │ 매칭          │  │
│                              └───────┬───────┘  │
│                                      │          │
│                          [Aggregation Group]    │
│                              │                  │
│                    ┌─────────┼─────────┐        │
│                    ▼         ▼         ▼        │
│              [Silence]  [Inhibit]  [TimeInterval]│
│                    └─────────┼─────────┘        │
│                              ▼                  │
│                   [Notification Pipeline]       │
│                    │    │    │    │              │
│                  Slack Email PD  Webhook ...     │
│                                                 │
│  [Cluster] ←──→ Gossip ←──→ [다른 Alertmanager] │
└─────────────────────────────────────────────────┘
```

## 교육 자료 목차

### 기본 문서

| # | 문서 | 내용 |
|---|------|------|
| 01 | [아키텍처](01-architecture.md) | 전체 아키텍처, 컴포넌트 관계, 초기화 흐름 |
| 02 | [데이터 모델](02-data-model.md) | Alert, Silence, Config 등 핵심 데이터 구조 |
| 03 | [시퀀스 다이어그램](03-sequence-diagrams.md) | 알림 수신→라우팅→전송 흐름 |
| 04 | [코드 구조](04-code-structure.md) | 디렉토리 구조, 빌드 시스템, 의존성 |
| 05 | [핵심 컴포넌트](05-core-components.md) | Dispatcher, Notifier, Silencer 동작 원리 |
| 06 | [운영 가이드](06-operations.md) | 배포, 설정, 모니터링, 트러블슈팅 |

### 심화 문서

| # | 문서 | 내용 |
|---|------|------|
| 07 | [Dispatcher](07-dispatcher.md) | Alert 라우팅과 Aggregation Group |
| 08 | [알림 파이프라인](08-notification-pipeline.md) | Stage 체인, Retry, Dedup |
| 09 | [Silence 관리](09-silence.md) | Silence CRUD, 매칭 인덱스, 클러스터 동기화 |
| 10 | [Inhibition](10-inhibition.md) | 억제 규칙, Source/Target 매칭 |
| 11 | [클러스터링](11-clustering.md) | Gossip 프로토콜, memberlist, 상태 동기화 |
| 12 | [Alert Provider](12-alert-provider.md) | 메모리 저장소, GC, 구독 모델 |
| 13 | [설정 관리](13-config-management.md) | Coordinator, 동적 리로드, Receiver 빌더 |
| 14 | [Notification Log](14-notification-log.md) | nflog, 중복 방지, 스냅샷 |
| 15 | [API v2](15-api-v2.md) | OpenAPI, go-swagger, 핸들러 구현 |
| 16 | [템플릿 엔진](16-template-engine.md) | Go 템플릿, 함수, Data 구조 |
| 17 | [Matcher 시스템](17-matcher-system.md) | 레이블 매칭, UTF-8 파서, 호환성 |
| 18 | [시간 간격 관리](18-time-intervals.md) | TimeInterval, 뮤트/활성 시간 |

### PoC (Proof of Concept)

| # | PoC | 시뮬레이션 대상 |
|---|-----|---------------|
| 1 | [poc-alert-routing](poc-alert-routing/) | Route 트리 매칭 알고리즘 |
| 2 | [poc-dispatcher](poc-dispatcher/) | Dispatcher와 Aggregation Group |
| 3 | [poc-notification-pipeline](poc-notification-pipeline/) | Stage 체인 파이프라인 |
| 4 | [poc-silence](poc-silence/) | Silence CRUD와 매칭 |
| 5 | [poc-inhibition](poc-inhibition/) | Inhibition 규칙 엔진 |
| 6 | [poc-gossip-cluster](poc-gossip-cluster/) | Gossip 프로토콜 클러스터링 |
| 7 | [poc-alert-store](poc-alert-store/) | 메모리 Alert 저장소와 GC |
| 8 | [poc-config-reload](poc-config-reload/) | 동적 설정 리로드 |
| 9 | [poc-nflog](poc-nflog/) | Notification Log |
| 10 | [poc-template](poc-template/) | 템플릿 엔진 |
| 11 | [poc-matcher](poc-matcher/) | 레이블 Matcher와 파서 |
| 12 | [poc-time-interval](poc-time-interval/) | 시간 간격 뮤팅 |
| 13 | [poc-alert-grouping](poc-alert-grouping/) | Alert 그룹핑 알고리즘 |
| 14 | [poc-deduplication](poc-deduplication/) | Alert 중복 제거 |
| 15 | [poc-rate-limiter](poc-rate-limiter/) | Rate Limiting (Bucket) |
| 16 | [poc-retry-backoff](poc-retry-backoff/) | 재시도와 Exponential Backoff |
