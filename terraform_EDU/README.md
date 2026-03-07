# Terraform 교육 자료 (EDU)

## 프로젝트 개요

Terraform은 HashiCorp이 개발한 **Infrastructure as Code(IaC)** 도구로, 선언적 설정 파일을 통해 인프라를 안전하고 효율적으로 구축, 변경, 버전 관리할 수 있다. HCL(HashiCorp Configuration Language)로 인프라를 정의하고, 실행 계획(Plan)을 미리 확인한 뒤 적용(Apply)하는 워크플로를 제공한다.

### 핵심 특징

| 특징 | 설명 |
|------|------|
| **Infrastructure as Code** | HCL로 인프라를 선언적으로 정의, 버전 관리 가능 |
| **실행 계획 (Execution Plan)** | `terraform plan`으로 변경 사항을 미리 확인 |
| **리소스 그래프 (Resource Graph)** | DAG 기반 의존성 분석으로 병렬 실행 최적화 |
| **변경 자동화 (Change Automation)** | 복잡한 인프라 변경을 자동화하여 휴먼 에러 최소화 |
| **프로바이더 플러그인** | AWS, GCP, Azure 등 다양한 클라우드를 플러그인으로 지원 |

### 아키텍처 핵심

```
사용자 (.tf 파일)
    ↓ HCL 파싱
설정(Config) → 리소스 그래프(DAG) 구축
    ↓ 상태(State)와 비교
실행 계획(Plan) 생성
    ↓ 사용자 승인
적용(Apply) → 프로바이더 플러그인(gRPC) → 클라우드 API
    ↓ 결과 반영
상태 파일(State) 업데이트
```

## 소스코드 정보

- **저장소**: https://github.com/hashicorp/terraform
- **언어**: Go
- **라이선스**: Business Source License 1.1
- **분석 소스 경로**: `/terraform/`

## 문서 목차

### 기본 문서

| 번호 | 문서 | 내용 |
|------|------|------|
| 01 | [아키텍처 개요](01-architecture.md) | 전체 아키텍처, 컴포넌트 관계, 초기화 흐름 |
| 02 | [데이터 모델](02-data-model.md) | 핵심 데이터 구조, State, Plan, Config 스키마 |
| 03 | [시퀀스 다이어그램](03-sequence-diagrams.md) | terraform plan/apply/init 주요 흐름 |
| 04 | [코드 구조](04-code-structure.md) | 디렉토리 구조, 빌드 시스템, 의존성 |
| 05 | [핵심 컴포넌트](05-core-components.md) | DAG 엔진, 프로바이더 시스템, 상태 관리 동작 원리 |
| 06 | [운영 가이드](06-operations.md) | 배포, 설정, 모니터링, 트러블슈팅 |

### 심화 문서

| 번호 | 문서 | 내용 |
|------|------|------|
| 07 | [DAG 그래프 엔진](07-dag-graph-engine.md) | DAG 구현, 위상 정렬, 병렬 워크, 이행적 축소 |
| 08 | [HCL 파싱 & 설정 로딩](08-hcl-config-loading.md) | HCL2 파싱, 모듈 트리, 변수 해석 |
| 09 | [프로바이더 플러그인 시스템](09-provider-plugin-system.md) | gRPC 프로토콜, 플러그인 라이프사이클, 스키마 캐싱 |
| 10 | [상태(State) 관리](10-state-management.md) | State 구조, SyncState, 백엔드별 저장, 잠금 |
| 11 | [Plan & Apply 엔진](11-plan-apply-engine.md) | 그래프 빌더, 변환기, 노드 실행, 변경 추적 |
| 12 | [모듈 시스템](12-module-system.md) | 모듈 설치, 레지스트리, 소스 유형, 매니페스트 |
| 13 | [백엔드 시스템](13-backend-system.md) | 로컬/리모트 백엔드, 상태 마이그레이션, 잠금 |
| 14 | [CLI 커맨드 시스템](14-cli-command-system.md) | 명령 등록, Meta 구조체, 오퍼레이션 디스패치 |
| 15 | [주소(Address) 체계](15-address-system.md) | 리소스/모듈/프로바이더 주소 파싱 및 해석 |
| 16 | [표현식 평가 & 내장 함수](16-expression-evaluation.md) | lang.Scope, EvalContext, 150+ 내장 함수 |
| 17 | [리팩토링 & Moved 블록](17-refactoring-moved-blocks.md) | moved/removed 블록, 상태 변환, 크로스 프로바이더 이동 |
| 18 | [테스트 프레임워크 & Stacks](18-testing-stacks.md) | terraform test, Stacks 아키텍처, HCP 통합 |

### PoC (Proof of Concept)

| 번호 | PoC | 핵심 개념 |
|------|-----|----------|
| 01 | [DAG 그래프 엔진](poc-dag-engine/) | DAG 구현, 위상 정렬, 병렬 워크 |
| 02 | [HCL 파서 시뮬레이터](poc-hcl-parser/) | HCL 문법 파싱, 블록 추출 |
| 03 | [리소스 그래프 빌더](poc-resource-graph/) | 리소스 의존성 그래프 구축 |
| 04 | [Plan 엔진](poc-plan-engine/) | Plan/Apply diff 계산 |
| 05 | [State 관리자](poc-state-manager/) | State 읽기/쓰기, 잠금, 직렬화 |
| 06 | [프로바이더 플러그인](poc-provider-plugin/) | gRPC 스타일 플러그인 통신 |
| 07 | [프로바이더 설치기](poc-provider-installer/) | 프로바이더 검색, 다운로드, 캐싱 |
| 08 | [모듈 로더](poc-module-loader/) | 모듈 의존성 해석, 설치, 매니페스트 |
| 09 | [백엔드 추상화](poc-backend-abstraction/) | 백엔드 인터페이스, 상태 저장, 잠금 |
| 10 | [CLI 디스패처](poc-cli-dispatcher/) | 커맨드 등록, 디스패치, 서브커맨드 |
| 11 | [주소 파서](poc-address-parser/) | 리소스/모듈 주소 파싱 |
| 12 | [표현식 평가기](poc-expression-evaluator/) | 변수 참조 해석, 내장 함수 |
| 13 | [이행적 축소](poc-transitive-reduction/) | 그래프 이행적 축소 알고리즘 |
| 14 | [Moved 블록 처리기](poc-moved-block/) | 리소스 이동, 상태 변환 |
| 15 | [동시성 그래프 워커](poc-concurrent-walker/) | 세마포어 기반 병렬 DAG 실행 |
| 16 | [Diff 계산기](poc-diff-calculator/) | 리소스 속성 비교, 변경 유형 판별 |

## 학습 가이드

### 입문자 (1~2일)
1. README.md → 01-architecture.md → 03-sequence-diagrams.md
2. PoC: poc-dag-engine, poc-hcl-parser, poc-plan-engine

### 중급자 (3~5일)
1. 02-data-model.md → 05-core-components.md → 04-code-structure.md
2. 심화: 07(DAG), 09(프로바이더), 10(State), 11(Plan/Apply)
3. PoC: poc-provider-plugin, poc-state-manager, poc-resource-graph

### 고급자 (1주+)
1. 06-operations.md + 모든 심화 문서
2. 소스코드 직접 분석 (internal/terraform/, internal/command/)
3. 모든 PoC 실행 및 변형 실험
