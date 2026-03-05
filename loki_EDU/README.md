# Grafana Loki EDU

## 프로젝트 개요

**"Like Prometheus, but for logs"** — Grafana Loki는 수평 확장 가능한 멀티테넌트 로그 집계 시스템이다.

Prometheus에서 영감을 받아 설계되었으며, 핵심 철학은 **로그 내용(content)을 인덱싱하지 않고 레이블 셋(label set)만 인덱싱**하는 것이다. 이 접근 방식은 전문 검색(full-text indexing) 기반 시스템 대비 스토리지 비용을 크게 절감하고, 운영 복잡도를 낮추면서도 Kubernetes 환경에서 강력한 로그 분석 기능을 제공한다.

| 항목 | 내용 |
|------|------|
| 소스 언어 | **Go** |
| 라이선스 | AGPL-3.0 |
| GitHub | [grafana/loki](https://github.com/grafana/loki) |
| 핵심 쿼리 언어 | LogQL (PromQL에서 영감) |

## 3개 컴포넌트 스택

Loki 에코시스템은 세 가지 주요 컴포넌트로 구성된다.

```
┌─────────────────────────────────────────────────────────┐
│                     Grafana (UI)                        │
│              로그 탐색, 대시보드, 알림 시각화               │
└─────────────────────┬───────────────────────────────────┘
                      │ LogQL 쿼리
                      ▼
┌─────────────────────────────────────────────────────────┐
│                   Loki (메인 서비스)                      │
│         로그 저장, 인덱싱, 쿼리 처리, 알림 규칙             │
└─────────────────────┬───────────────────────────────────┘
                      ▲ Push API (HTTP/gRPC)
                      │
┌─────────────────────────────────────────────────────────┐
│                Alloy (에이전트, 구 Promtail)              │
│           로그 수집, 레이블 부착, 전처리, 전송              │
└─────────────────────────────────────────────────────────┘
```

- **Alloy (에이전트)**: 각 노드에서 로그를 수집하고, 레이블을 부착하여 Loki로 전송하는 경량 에이전트. 기존 Promtail을 대체하는 Grafana Alloy가 권장 에이전트이다.
- **Loki (메인 서비스)**: 로그 저장, 인덱싱, 쿼리 처리를 담당하는 핵심 서비스. 멀티테넌트를 지원하며, 수평 확장이 가능하다.
- **Grafana (UI)**: 로그 탐색, 대시보드 구성, 알림 시각화를 위한 웹 인터페이스. Loki를 데이터소스로 네이티브 지원한다.

## 주요 특징

| 특징 | 설명 |
|------|------|
| **비용 효율적** | 로그 내용을 인덱싱하지 않아 스토리지/컴퓨팅 비용이 낮음. 오브젝트 스토리지(S3, GCS 등)에 압축 저장 |
| **운영 간편** | 단일 바이너리 배포 가능, 설정이 간결하며 Prometheus와 유사한 운영 모델 |
| **Kubernetes 네이티브** | Pod 레이블 자동 디스커버리, Helm 차트 제공, Kubernetes 메타데이터 활용 |
| **Grafana 통합** | Grafana에서 메트릭-로그-트레이스를 하나의 UI에서 상관 분석 가능 |
| **수평 확장** | 읽기/쓰기 경로 분리, 각 컴포넌트 독립 스케일링 가능 |
| **멀티테넌트** | 테넌트별 데이터 격리, 리소스 제한, 인증/인가 지원 |

## 아키텍처

Loki는 **모노리스/마이크로서비스 하이브리드** 아키텍처를 채택한다. 단일 바이너리에 모든 컴포넌트가 포함되어 있으며, `-target` 플래그로 실행할 컴포넌트를 선택한다.

### 배포 모드

```
단일 바이너리 (모노리스)          마이크로서비스 모드
┌─────────────────────┐     ┌──────────┐ ┌──────────┐
│  -target=all        │     │-target=  │ │-target=  │
│                     │     │distributor│ │ingester  │
│ ┌─────┐ ┌────────┐ │     └──────────┘ └──────────┘
│ │Dist │ │Ingester│ │     ┌──────────┐ ┌──────────┐
│ └─────┘ └────────┘ │     │-target=  │ │-target=  │
│ ┌─────┐ ┌────────┐ │     │querier   │ │compactor │
│ │Query│ │Compact │ │     └──────────┘ └──────────┘
│ └─────┘ └────────┘ │
└─────────────────────┘
```

- **모듈 시스템**: 각 컴포넌트(Distributor, Ingester, Querier, Compactor 등)는 독립 모듈로 구현되어 있으며, 의존성 그래프에 따라 자동으로 초기화/종료된다.
- **Ring 기반 서비스 디스커버리**: Consistent Hash Ring을 사용하여 데이터 분산과 복제를 관리한다. Ingester, Distributor 등이 Ring에 등록되어 동적으로 클러스터 멤버십을 관리한다.

### 쓰기/읽기 경로

```
쓰기 경로 (Write Path)                    읽기 경로 (Read Path)

Alloy → Distributor → Ingester           Grafana → Query Frontend
              │            │                           │
              │            ▼                     Query Scheduler
              │        WAL + Memory                    │
              │            │                       Querier
              ▼            ▼                      ↙        ↘
        Validation    Object Storage         Ingester    Store Gateway
                      (S3/GCS/...)              │            │
                                            Memory      Object Storage
```

## EDU 문서 목차

### 기본 문서 (7개)

| 문서 | 내용 |
|------|------|
| **README.md** | 프로젝트 개요, EDU 목차 (본 문서) |
| **[01-architecture.md](01-architecture.md)** | 전체 아키텍처, 컴포넌트 관계, 초기화 흐름, 모듈 시스템 |
| **[02-data-model.md](02-data-model.md)** | 핵심 데이터 구조, 청크 포맷, 인덱스 스키마, API 스펙 |
| **[03-sequence-diagrams.md](03-sequence-diagrams.md)** | 주요 유즈케이스의 요청 흐름 (로그 쓰기, 쿼리 실행) |
| **[04-code-structure.md](04-code-structure.md)** | 디렉토리 구조, 빌드 시스템, 의존성 관리 |
| **[05-core-components.md](05-core-components.md)** | 핵심 컴포넌트 동작 원리 (Distributor, Ingester, Querier 등) |
| **[06-operations.md](06-operations.md)** | 배포, 설정, 모니터링, 트러블슈팅 |

### 심화 문서 (12개)

| 문서 | 내용 |
|------|------|
| **[07-logql-engine.md](07-logql-engine.md)** | LogQL 쿼리 엔진 — 파싱, AST, 실행 계획, 최적화 |
| **[08-distributor.md](08-distributor.md)** | 분산 로그 수집기 — 검증, 전처리, Ring 기반 분배, 레이트 리밋 |
| **[09-ingester.md](09-ingester.md)** | 인메모리 로그 저장소 — 스트림 관리, WAL, 청크 플러시, 핸드오프 |
| **[10-storage.md](10-storage.md)** | 스토리지 레이어 — 청크 인코딩, 오브젝트 스토어, TSDB 인덱스, 캐싱 |
| **[11-querier.md](11-querier.md)** | 쿼리 실행기 — 쿼리 프론트엔드, 스케줄러, 병렬 처리, 반복자 병합 |
| **[12-compactor.md](12-compactor.md)** | 인덱스 압축기 — 인덱스 병합, 테넌트별 압축, 보존 정책, 삭제 처리 |
| **[13-bloom-filters.md](13-bloom-filters.md)** | 블룸 필터 서브시스템 — 블룸 빌더, 게이트웨이, 쿼리 가속 |
| **[14-pattern-detection.md](14-pattern-detection.md)** | 로그 패턴 감지 — Drain 알고리즘, 패턴 인제스터, 메트릭 생성 |
| **[15-kafka-integration.md](15-kafka-integration.md)** | Kafka 연동 — 파티션 기반 수집, 소비자 그룹, 블록 빌더 |
| **[16-ruler.md](16-ruler.md)** | 알림 규칙 엔진 — 규칙 평가, Alertmanager 연동, 녹화 규칙 |
| **[17-index-gateway.md](17-index-gateway.md)** | 인덱스 게이트웨이 — 인덱스 캐싱, Querier 오프로드, 샤딩 |
| **[18-dataobj.md](18-dataobj.md)** | 컬럼나 데이터 포맷 — DataObj 구조, 섹션 레이아웃, 인코딩 |

### PoC 코드 (18개)

모든 PoC는 Go 표준 라이브러리만 사용하며, `go run main.go`로 실행할 수 있다.

| PoC | 주제 | 설명 |
|-----|------|------|
| **[poc-01-module-system](poc-01-module-system/)** | 모듈 시스템 | 의존성 그래프 기반 모듈 초기화/종료 시뮬레이션 |
| **[poc-02-ring-hash](poc-02-ring-hash/)** | Ring 해시 | Consistent Hash Ring 기반 서비스 디스커버리 구현 |
| **[poc-03-log-pipeline](poc-03-log-pipeline/)** | 로그 파이프라인 | 수집 → 전처리 → 분배 → 저장 파이프라인 시뮬레이션 |
| **[poc-04-chunk-encoding](poc-04-chunk-encoding/)** | 청크 인코딩 | 로그 청크의 압축/인코딩 메커니즘 구현 |
| **[poc-05-memchunk](poc-05-memchunk/)** | 인메모리 청크 | 인메모리 로그 청크 저장 및 반복자 패턴 구현 |
| **[poc-06-tsdb-index](poc-06-tsdb-index/)** | TSDB 인덱스 | 레이블 기반 시계열 인덱스 구조 시뮬레이션 |
| **[poc-07-logql-parser](poc-07-logql-parser/)** | LogQL 파서 | LogQL 쿼리 파싱 및 AST 생성 구현 |
| **[poc-08-iterator-merge](poc-08-iterator-merge/)** | 반복자 병합 | 다중 소스 로그 스트림의 정렬 병합 구현 |
| **[poc-09-rate-limiter](poc-09-rate-limiter/)** | 레이트 리미터 | 테넌트별 수집 속도 제한 메커니즘 구현 |
| **[poc-10-wal](poc-10-wal/)** | WAL | Write-Ahead Log 기반 내구성 보장 구현 |
| **[poc-11-query-frontend](poc-11-query-frontend/)** | 쿼리 프론트엔드 | 쿼리 분할, 큐잉, 캐싱 시뮬레이션 |
| **[poc-12-bloom-filter](poc-12-bloom-filter/)** | 블룸 필터 | 블룸 필터 기반 로그 검색 가속 구현 |
| **[poc-13-drain-pattern](poc-13-drain-pattern/)** | Drain 패턴 | Drain 알고리즘 기반 로그 패턴 자동 감지 구현 |
| **[poc-14-kafka-consumer](poc-14-kafka-consumer/)** | Kafka 소비자 | 파티션 기반 로그 소비 및 블록 빌더 시뮬레이션 |
| **[poc-15-compaction](poc-15-compaction/)** | 압축 | 인덱스/청크 압축 및 보존 정책 적용 구현 |
| **[poc-16-multi-tenant](poc-16-multi-tenant/)** | 멀티테넌트 | 테넌트 격리, 리소스 제한, 데이터 분리 구현 |
| **[poc-17-dataobj-columnar](poc-17-dataobj-columnar/)** | 컬럼나 포맷 | DataObj 컬럼나 데이터 포맷 인코딩/디코딩 구현 |
| **[poc-18-canary-monitor](poc-18-canary-monitor/)** | 카나리 모니터 | Loki Canary 기반 로그 파이프라인 헬스체크 구현 |

## 학습 로드맵

```
[입문] README → 01-architecture → 04-code-structure → 06-operations
  │
  ▼
[기본] 02-data-model → 03-sequence-diagrams → 05-core-components
  │
  ▼
[심화 - 쓰기 경로] 08-distributor → 09-ingester → 10-storage
  │
  ▼
[심화 - 읽기 경로] 07-logql-engine → 11-querier → 17-index-gateway
  │
  ▼
[심화 - 고급] 12-compactor → 13-bloom-filters → 14-pattern-detection
  │
  ▼
[심화 - 확장] 15-kafka-integration → 16-ruler → 18-dataobj
```

## 참고 자료

- [Grafana Loki 공식 문서](https://grafana.com/docs/loki/latest/)
- [Grafana Loki GitHub](https://github.com/grafana/loki)
- [LogQL 문서](https://grafana.com/docs/loki/latest/logql/)
- [Loki 아키텍처 문서](https://grafana.com/docs/loki/latest/get-started/architecture/)
