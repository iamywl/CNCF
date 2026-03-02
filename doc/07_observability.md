# Observability (관측성)

시스템의 내부 상태를 외부에서 이해할 수 있게 하는 도구들.
3대 축: **Logging(로그)**, **Metrics(메트릭)**, **Tracing(트레이싱)**.

---

## 3대 축 개요

```
앱/컨테이너
  ├── 로그(Logs)     → "무슨 일이 일어났는가?" (이벤트 기록)
  ├── 메트릭(Metrics) → "얼마나 바쁜가?" (수치 측정)
  └── 트레이스(Traces) → "어디서 느려졌는가?" (요청 경로 추적)
        │
        ├── 계측 표준: OpenTelemetry (3가지를 통합)
        │
        └── 시각화: Grafana (전부 한 화면에)
```

| 축 | 질문 | 데이터 형태 | 대표 도구 |
|---|---|---|---|
| Logging | 무슨 일이 일어났는가? | 텍스트 이벤트 | Fluentd, Loki |
| Metrics | 얼마나 바쁜가? | 시계열 수치 | Prometheus |
| Tracing | 어디서 느려졌는가? | 요청 경로(span) | Jaeger |

---

## Logging (로그)

### Graduated

#### Fluentd ★ (CNCF Graduated)
- **역할**: 로그 수집/변환/전달 파이프라인
- **구조**: Input(소스) → Filter(변환) → Output(목적지)
- **특징**: 600+ 플러그인. 컨테이너, 파일, syslog, HTTP 등 다양한 소스 지원
- **출력**: Elasticsearch, S3, Kafka, Splunk 등
- **비유**: 우체국 — 여기저기서 편지(로그)를 모아서 목적지로 배달

#### Fluent Bit ★ (CNCF Graduated)
- **역할**: Fluentd의 경량 버전
- **차이**: C로 작성, 메모리 ~450KB. Fluentd(Ruby)보다 10배 가벼움
- **적합**: 엣지, IoT, 사이드카 등 리소스가 제한된 환경
- **비유**: 우체국의 오토바이 배달원 — 작고 빠르게 로그를 수거

#### Fluentd vs Fluent Bit

| | Fluentd | Fluent Bit |
|---|---|---|
| 언어 | Ruby + C | C |
| 메모리 | ~60MB | ~450KB |
| 플러그인 | 600+ | 100+ |
| 용도 | 중앙 집계 서버 | 에이전트/사이드카 |
| 보통 조합 | Fluent Bit(수집) → Fluentd(집계) → Elasticsearch |

### 주요 비-CNCF 도구

| 도구 | 설명 |
|---|---|
| **Loki** | Grafana의 로그 저장소. Prometheus와 같은 레이블 기반 인덱싱. 저비용으로 대량 로그 저장 |
| **Elastic (ELK Stack)** | Elasticsearch + Logstash + Kibana. 가장 전통적이고 강력한 로그 분석 스택 |
| **Splunk** | 엔터프라이즈 로그 분석 플랫폼 (상용) |
| **Vector** | Datadog의 고성능 로그/메트릭 파이프라인 (Rust) |

---

## Monitoring (메트릭 모니터링)

### Graduated

#### Prometheus ★ (CNCF Graduated)
- **역할**: 메트릭 수집/저장/쿼리 시스템 (**업계 표준**)
- **동작**: Pull 방식 — Prometheus가 주기적으로 대상에서 메트릭을 긁어옴(scrape)
- **쿼리**: PromQL (Prometheus Query Language)
- **알림**: Alertmanager와 연동하여 임계값 초과 시 Slack/PagerDuty 등으로 알림
- **제약**: 단일 노드, 장기 저장 어려움 → Thanos/Cortex로 보완
- **비유**: 자동차 계기판 — 속도(RPS), RPM(CPU), 온도(메모리)를 실시간 표시
- **예시**:
  ```promql
  # 5분간 평균 CPU 사용률
  rate(container_cpu_usage_seconds_total[5m])

  # HTTP 요청 에러율
  rate(http_requests_total{status=~"5.."}[5m]) / rate(http_requests_total[5m])
  ```

### Incubating

#### Cortex ★ (CNCF Incubating)
- **역할**: Prometheus 멀티테넌트 장기 저장소
- **왜 쓰나**: 여러 Prometheus의 메트릭을 수평 확장 가능한 클러스터에 장기 저장
- **특징**: 멀티테넌트 (팀별 격리), 수평 확장, S3/GCS 백엔드 저장
- **비유**: Prometheus가 수첩이면, Cortex는 창고형 보관소

#### Thanos ★ (CNCF Incubating)
- **역할**: Prometheus 장기 저장 + HA(High Availability) + 글로벌 뷰
- **왜 쓰나**: 여러 클러스터의 Prometheus를 하나의 쿼리로 조회
- **구조**: 기존 Prometheus에 사이드카를 붙여서 확장 (Cortex보다 도입이 쉬움)
- **비유**: 여러 지점의 CCTV를 본사 통합 관제실에서 보는 것

#### Cortex vs Thanos

| | Cortex | Thanos |
|---|---|---|
| 아키텍처 | 중앙 집중 | 사이드카 분산 |
| 도입 난이도 | 높음 | 낮음 (기존 Prometheus에 붙임) |
| 멀티테넌트 | 네이티브 지원 | 제한적 |
| 적합 | 대규모 SaaS, 멀티테넌트 | 멀티클러스터 통합 뷰 |

### 주요 비-CNCF 도구

| 도구 | 설명 |
|---|---|
| **Grafana** | 시각화 대시보드 (**사실상 표준**). Prometheus, Jaeger, Loki 등 거의 모든 데이터소스 연동 |
| **Datadog** | 올인원 관측성 SaaS (메트릭 + 로그 + 트레이싱 + APM) |
| **New Relic** | 올인원 관측성 SaaS |
| **Zabbix** | 전통적 인프라 모니터링 (에이전트 기반) |
| **VictoriaMetrics** | Prometheus 호환 장기 저장소. Thanos/Cortex 대안 (단일 바이너리, 고성능) |

---

## Tracing (분산 트레이싱)

### Graduated

#### Jaeger ★ (CNCF Graduated)
- **역할**: 분산 트레이싱 시스템 (Uber에서 개발)
- **핵심 개념**:
  - **Trace**: 하나의 요청이 시스템을 통과하는 전체 경로
  - **Span**: Trace 안의 개별 작업 단위 (서비스 A → 서비스 B 호출 = 1 Span)
- **왜 쓰나**: 마이크로서비스에서 "이 요청이 어디서 느려졌나?"를 시각화
- **비유**: 택배 추적 시스템 — 택배가 어디를 거쳤고, 각 단계에서 얼마나 걸렸는지 추적

### Incubating

#### OpenTelemetry ★ (CNCF Incubating)
- **역할**: 로그/메트릭/트레이싱을 하나의 표준 SDK/API로 통합
- **왜 쓰나**: 앱에 OpenTelemetry SDK를 한 번 넣으면, 데이터를 Jaeger든 Prometheus든 어디든 보낼 수 있음
- **구성**:
  - **API**: 계측(instrumentation) 인터페이스
  - **SDK**: 데이터 수집/처리 구현체
  - **Collector**: 데이터 수신 → 처리 → 내보내기 파이프라인
- **벤더 종속 제거**: 관측성 도구를 바꿔도 앱 코드 변경 불필요
- **비유**: 만능 어댑터 — 전원 플러그(계측) 규격을 하나로 통일
- **지원 언어**: Java, Python, Go, JavaScript, .NET, C++, Rust 등

---

## 전체 관측성 아키텍처 예시

```
[앱/컨테이너]
    │
    ├── OpenTelemetry SDK (통합 계측)
    │       │
    │       ├── Traces  ──→ OTel Collector ──→ Jaeger (저장/쿼리)
    │       ├── Metrics ──→ OTel Collector ──→ Prometheus (저장/쿼리)
    │       └── Logs    ──→ OTel Collector ──→ Loki (저장/쿼리)
    │
    ├── Fluent Bit (로그 수집 에이전트)
    │       └──→ Fluentd (집계) ──→ Elasticsearch
    │
    └── Prometheus (메트릭 Pull)
            └──→ Thanos/Cortex (장기 저장)
                    │
                    └──→ Grafana (시각화 대시보드)
                            ├── Prometheus 데이터소스
                            ├── Jaeger 데이터소스
                            ├── Loki 데이터소스
                            └── Elasticsearch 데이터소스
```

---

## Grafana 생태계 (비-CNCF, 하지만 사실상 표준)

Grafana Labs가 관측성 3대 축 전체를 커버하는 오픈소스 스택을 제공:

| 도구 | 역할 | 대응하는 전통 스택 |
|---|---|---|
| **Grafana** | 시각화 대시보드 | Kibana |
| **Prometheus** | 메트릭 수집 (Grafana가 공식 지원) | Graphite |
| **Loki** | 로그 저장/쿼리 | Elasticsearch |
| **Tempo** | 트레이스 저장/쿼리 | Jaeger |
| **Mimir** | Prometheus 장기 저장 (Cortex 포크) | Thanos |
| **k6** | 부하 테스트 | JMeter |
| **Alloy** | 텔레메트리 수집 에이전트 | Fluent Bit |

이 조합을 "**LGTM Stack**"이라 부른다: **L**oki + **G**rafana + **T**empo + **M**imir
