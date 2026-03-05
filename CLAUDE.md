# CNCF 프로젝트

오픈소스 프로젝트 소스코드 + 교육 자료(EDU) 모노레포.

## 디렉토리 구조

```
CNCF/
├── doc/                    # 프로젝트 문서, 작업 현황
│   └── EDU_WORK_STATUS.md  # EDU 작업 진행 현황 및 스케줄
├── {프로젝트명}_EDU/         # 교육 자료 (문서 + PoC)
│   ├── README.md
│   ├── 01~06-*.md          # 기본 문서 (아키텍처, 데이터모델, 시퀀스, 코드구조, 핵심컴포넌트, 운영)
│   ├── 07~18-*.md          # 심화 문서 (서브시스템별 deep-dive)
│   └── poc-*/main.go       # PoC 코드 (go run main.go 실행 가능, 외부 의존성 없음)
├── alertmanager/           # [소스] Prometheus Alertmanager
├── argo-cd/                # [소스] Argo CD
├── containerd/             # [소스] containerd
├── grafana/                # [소스] Grafana
├── grpc-go/                # [소스] gRPC-Go
├── helm/                   # [소스] Helm
├── jenkins/                # [소스] Jenkins
├── kafka/                  # [소스] Apache Kafka
├── loki/                   # [소스] Grafana Loki
├── tart/                   # [소스] Tart (macOS VM)
└── terraform/              # [소스] Terraform
```

## EDU 작업 순서

| 순서 | 프로젝트 | EDU 디렉토리 | 소스 언어 | 비고 |
|------|----------|-------------|----------|------|
| 1 | cilium | cilium_EDU | Go/C | 사용자 지정 |
| 2 | kubernetes | kubernetes_EDU | Go | 사용자 지정 |
| 3 | argo-cd | argo-cd_EDU | Go | 사용자 지정 |
| 4 | jenkins | jenkins_EDU | Java | 사용자 지정 |
| 5 | containerd | containerd_EDU | Go | 사용자 지정 |
| 6 | grpc-go | grpc-go_EDU | Go | 사용자 지정 |
| 7 | tart | tart_EDU | Swift | 사용자 지정 |
| 8 | helm | helm_EDU | Go | 자율 |
| 9 | terraform | terraform_EDU | Go | 자율 |
| 10 | alertmanager | alertmanager_EDU | Go | 자율 |
| 11 | hubble | hubble_EDU | Go | 자율 |
| 12 | kafka | kafka_EDU | Java/Scala | 자율 |
| 13 | grafana | grafana_EDU | Go/TS | 자율 |
| 14 | loki | loki_EDU | Go | 자율 |

## EDU 소스코드 분석 절차 (필수)

각 프로젝트 EDU 작성 전에 아래 절차를 **반드시** 순서대로 수행한다.
문서/PoC에 포함되는 모든 소스코드 참조는 이 절차에서 직접 확인한 것만 사용한다.

### 1단계: 프로젝트 개요 파악 (→ README.md, 01-architecture.md에 반영)
- README.md, CONTRIBUTING.md, docs/ 읽기
- 프로젝트가 해결하는 문제, 핵심 기능 정리
- 아키텍처 문서가 있으면 반드시 확인

### 2단계: 디렉토리 구조 파악 (→ 04-code-structure.md에 반영)
- `tree -L 2 -d`로 전체 구조 확인
- cmd/ → 진입점, pkg/internal/ → 핵심 로직, api/ → API 정의, test/ → 테스트
- 빌드 시스템 확인 (Makefile, go.mod, pom.xml, Package.swift 등)

### 3단계: 진입점(Entry Point) 추적 (→ 01-architecture.md, 03-sequence-diagrams.md에 반영)
- main() 함수 찾기 — 프로그램 시작점
- 초기화 흐름 따라가기 — 설정 로드 → 의존성 주입 → 서버 시작
- 핵심 루프/핸들러 확인 — 실제 요청 처리 부분

### 4단계: 데이터 모델 이해 (→ 02-data-model.md에 반영)
- 핵심 struct/class/type 정의 찾기 (Grep으로 검색)
- DB 스키마, API 스펙 (protobuf, OpenAPI) 확인
- 데이터 흐름 추적

### 5단계: 핵심 흐름(Critical Path) 분석 (→ 03-sequence-diagrams.md, 05-core-components.md에 반영)
- 주요 유즈케이스 1~2개를 골라서 코드를 끝까지 따라가기
- 요청 → 핸들러 → 비즈니스 로직 → 저장소 → 응답
- 에러 처리, 동시성 패턴 확인

### 6단계: 서브시스템 심화 분석 (→ 07~18 심화문서에 반영)
- 각 서브시스템의 핵심 파일을 Read로 직접 읽기
- "왜(Why)" 질문: 이 설계를 선택한 이유, 이 패턴을 쓰는 이유
- 코드에서 발견한 실제 경로/함수명/구조체만 인용

### 7단계: PoC 작성 (→ poc-*/main.go에 반영)
- 서브시스템의 핵심 개념을 Go 표준 라이브러리만으로 시뮬레이션
- 실제 소스코드의 동작 원리를 재현 (피상적 모방이 아닌 핵심 알고리즘 구현)
- `go run main.go` 실행 검증

## EDU 문서 구조

| 문서 | 분석 단계 | 내용 |
|------|----------|------|
| README.md | 1단계 | 프로젝트 개요, EDU 목차 |
| 01-architecture.md | 1,3단계 | 전체 아키텍처, 컴포넌트 관계, 초기화 흐름 |
| 02-data-model.md | 4단계 | 핵심 데이터 구조, 스키마, API 스펙 |
| 03-sequence-diagrams.md | 3,5단계 | 주요 유즈케이스의 요청 흐름 |
| 04-code-structure.md | 2단계 | 디렉토리 구조, 빌드 시스템, 의존성 |
| 05-core-components.md | 5단계 | 핵심 컴포넌트 동작 원리 |
| 06-operations.md | 1단계 | 배포, 설정, 모니터링, 트러블슈팅 |
| 07~18-*.md | 6단계 | 서브시스템별 deep-dive (각 500줄 이상) |

## EDU 품질 기준

| 항목 | 기준 |
|------|------|
| 기본문서 | 6~7개 (README + 01~06) |
| 심화문서 | 10~12개 (서브시스템별 deep-dive, 각 500줄 이상) |
| PoC | 16~18개 (각 main.go + README.md, 외부 의존성 없음) |
| 언어 | 전체 한국어 |
| 심화문서 내용 | 검증된 코드 참조, ASCII 다이어그램, Mermaid, 테이블, "왜(Why)" 중심 설명 |

## 작업 규칙

1. **한 번에 하나의 프로젝트만** — 토큰 절약, 품질 확보 우선
2. **위 분석 절차를 반드시 따를 것** — 1~7단계 순서대로 소스코드를 충분히 읽은 뒤 문서 작성
3. **소스코드 참조는 검증 필수** — 추측으로 파일 경로/함수명 적지 말 것, Grep/Read로 직접 확인한 것만 인용
4. **PoC는 실행 검증** — `go run main.go` 실행 가능 확인, 표준 라이브러리만 사용
5. 작업 현황은 doc/EDU_WORK_STATUS.md에 기록
6. `/compact`, `/clear` 적극 활용하여 컨텍스트 관리

## GitHub

- Remote: https://github.com/iamywl/CNCF.git
- Branch: main
