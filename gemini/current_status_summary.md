# CNCF EDU 프로젝트 현재 상황 및 향후 계획 요약

## 1. 프로젝트 진행 요약 (2026-03-06)

*   **전체 프로젝트 수:** 16개
*   **완료 (11개):** Cilium, Kubernetes, Argo CD, Jenkins, Containerd, gRPC-Go, Tart, Helm, Terraform, Hubble, Jaeger
*   **진행 중 (1개):** Istio (긴급 우선 진행 중)
*   **미시작 (4개):** Alertmanager, Kafka, Grafana, Loki

## 2. 작업 품질 및 구성 (프로젝트당)

*   **기본 문서 (7개):** 아키텍처, 데이터 모델, 시퀀스 다이어그램 등
*   **심화 문서 (10~12개):** 서브시스템별 Deep-dive (각 500줄 이상)
*   **PoC (16~18개):** Go 표준 라이브러리 기반 실행 가능한 코드

## 3. 토큰 소모 및 리소스 추산

*   **프로젝트당 토큰:** 약 450k ~ 900k tokens
*   **주요 소모처:** 코드베이스 전체 분석(Research) 및 500줄 이상의 상세 문서 생성(Execution)

## 4. 향후 작업 순서

1.  **Istio (진행 중):** 서비스 메시 핵심 로직 및 Envoy 연동 분석
2.  **Alertmanager:** 프로메테우스 알람 핸들링 및 디스패치 로직
3.  **Kafka:** Java/Scala 기반의 분산 메시징 시스템 분석 (난이도 높음)
4.  **Grafana:** Go/TS 기반의 대시보드 및 데이터 소스 엔진
5.  **Loki:** 로그 수집 및 인덱싱 구조 분석
