# Jaeger 교육 자료 (EDU)

## 프로젝트 개요

Jaeger는 **분산 트레이싱 플랫폼**으로, Uber Technologies에서 개발하여 CNCF에 기증한 오픈소스 프로젝트이다. 2019년 10월 CNCF Graduated 프로젝트로 승격되었으며, 마이크로서비스 아키텍처에서 요청 흐름을 추적하고 성능 병목을 분석하는 데 사용된다.

### 핵심 기능
- **분산 트레이싱**: 마이크로서비스 간 요청 흐름을 end-to-end로 추적
- **성능 분석**: 서비스 간 지연(latency) 원인 파악 및 병목 식별
- **서비스 의존성 분석**: 서비스 간 호출 관계 시각화
- **적응형 샘플링**: 트래픽 기반 동적 샘플링 확률 조정
- **다중 스토리지 백엔드**: Memory, Badger, Cassandra, Elasticsearch, ClickHouse 지원
- **OpenTelemetry 네이티브**: OTLP 프로토콜 직접 수신, OTel Collector 기반 아키텍처

### Jaeger v2 아키텍처
Jaeger v2는 **OpenTelemetry Collector** 위에 구축된 완전히 새로운 아키텍처를 채택했다. 모든 컴포넌트(Collector, Query, Storage)가 OTel Collector의 Extension/Receiver/Exporter/Processor 패턴으로 구현되어 있다.

### 기술 스택
- **언어**: Go
- **프로토콜**: OTLP (gRPC/HTTP), Jaeger Thrift, Zipkin
- **프레임워크**: OpenTelemetry Collector
- **스토리지**: Memory, Badger, Cassandra, Elasticsearch/OpenSearch, ClickHouse
- **라이선스**: Apache 2.0

---

## 문서 목차

### 기본 문서
| 번호 | 문서 | 내용 |
|------|------|------|
| 01 | [아키텍처](01-architecture.md) | 전체 아키텍처, OTel Collector 기반 설계, 컴포넌트 관계 |
| 02 | [데이터 모델](02-data-model.md) | Span, Trace, Process 구조, OTLP/Jaeger 모델 변환 |
| 03 | [시퀀스 다이어그램](03-sequence-diagrams.md) | 트레이스 수집/조회/샘플링 흐름 |
| 04 | [코드 구조](04-code-structure.md) | 디렉토리 구조, 빌드 시스템, 의존성 |
| 05 | [핵심 컴포넌트](05-core-components.md) | Storage Extension, Query Extension, Sampling 동작 원리 |
| 06 | [운영](06-operations.md) | 배포, 설정, 모니터링, 트러블슈팅 |

### 심화 문서
| 번호 | 문서 | 내용 |
|------|------|------|
| 07 | [스토리지 아키텍처](07-storage-architecture.md) | Factory 패턴, V1/V2 API, 지연 초기화 |
| 08 | [메모리 & Badger 스토리지](08-memory-badger-storage.md) | 인메모리/임베디드 스토리지 구현 |
| 09 | [Cassandra 스토리지](09-cassandra-storage.md) | 파티셔닝, 인덱스, Duration 쿼리 전략 |
| 10 | [Elasticsearch 스토리지](10-elasticsearch-storage.md) | 인덱스 관리, 매핑, 롤오버 |
| 11 | [ClickHouse 스토리지](11-clickhouse-storage.md) | 컬럼 스토어 설계, 스키마 |
| 12 | [Query 서비스](12-query-service.md) | HTTP/gRPC API, UI 서빙, 트레이스 조회 |
| 13 | [샘플링 시스템](13-sampling-system.md) | 파일 기반/적응형 샘플링 알고리즘 |
| 14 | [OTel Collector 통합](14-otel-collector-integration.md) | Receiver, Exporter, Processor, Extension |
| 15 | [MCP 서버](15-mcp-server.md) | Model Context Protocol, LLM 통합 |
| 16 | [멀티테넌시 & 인증](16-multitenancy-auth.md) | 테넌시 관리, 인증/인가 |
| 17 | [HotROD 데모 앱](17-hotrod-demo.md) | 데모 애플리케이션 아키텍처 |
| 18 | [운영 도구](18-operational-tools.md) | ES 인덱스 관리, 트레이스 생성기, 익명화 |

### PoC (Proof of Concept)
| 번호 | PoC | 핵심 개념 |
|------|-----|----------|
| 01 | [poc-span-model](poc-span-model/) | Span/Trace 데이터 모델 시뮬레이션 |
| 02 | [poc-trace-collector](poc-trace-collector/) | OTLP 기반 트레이스 수집 파이프라인 |
| 03 | [poc-storage-factory](poc-storage-factory/) | Factory 패턴 스토리지 추상화 |
| 04 | [poc-memory-store](poc-memory-store/) | 인메모리 스토리지 구현 |
| 05 | [poc-inverted-index](poc-inverted-index/) | 역인덱스 기반 트레이스 검색 |
| 06 | [poc-query-service](poc-query-service/) | HTTP API 트레이스 조회 서비스 |
| 07 | [poc-sampling-strategy](poc-sampling-strategy/) | 확률적/레이트 리미팅 샘플링 |
| 08 | [poc-adaptive-sampling](poc-adaptive-sampling/) | 적응형 샘플링 알고리즘 |
| 09 | [poc-dag-dependency](poc-dag-dependency/) | 서비스 의존성 DAG 분석 |
| 10 | [poc-batch-processor](poc-batch-processor/) | 배치 프로세서 파이프라인 |
| 11 | [poc-trace-pipeline](poc-trace-pipeline/) | Receiver→Processor→Exporter 파이프라인 |
| 12 | [poc-critical-path](poc-critical-path/) | 크리티컬 패스 분석 알고리즘 |
| 13 | [poc-duration-index](poc-duration-index/) | 시간 버킷 기반 Duration 인덱스 |
| 14 | [poc-tenant-isolation](poc-tenant-isolation/) | 멀티테넌트 트레이스 격리 |
| 15 | [poc-trace-anonymizer](poc-trace-anonymizer/) | 트레이스 데이터 익명화 |
| 16 | [poc-leader-election](poc-leader-election/) | 분산 리더 선출 메커니즘 |

---

## 소스코드 참조

- **소스 위치**: `/Users/ywlee/sideproejct/CNCF/jaeger/`
- **버전**: Jaeger v2 (OpenTelemetry Collector 기반)
- **GitHub**: https://github.com/jaegertracing/jaeger
