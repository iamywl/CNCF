# EDU - 소프트웨어 문서화 학습 가이드

소프트웨어 개발에서 문서화 전략과 요소 기술을 실습할 수 있는 교육 프로젝트입니다.
각 디렉토리는 독립적인 POC(Proof of Concept) 프로젝트로 구성되어 있어,
직접 실행하며 학습할 수 있습니다.

## 계층별 구조

### 상위 수준 (Architecture & Design)

| 번호 | 디렉토리 | 설명 |
|------|----------|------|
| 01 | [architecture-diagram](01-architecture-diagram/) | 시스템 아키텍처 다이어그램 (C4 모델, 마이크로서비스) |
| 02 | [erd](02-erd/) | Entity Relationship Diagram + SQL DDL |
| 03 | [sequence-diagram](03-sequence-diagram/) | 시퀀스 다이어그램으로 흐름 표현 |

### 하위 수준 (Code Level)

| 번호 | 디렉토리 | 설명 |
|------|----------|------|
| 04 | [api-documentation](04-api-documentation/) | Swagger/OpenAPI 기반 API 명세 (Node.js) |
| 05 | [jsdoc](05-jsdoc/) | JSDoc 자동 문서 생성 (Node.js) |
| 06 | [pydoc](06-pydoc/) | Python Docstring & 자동 문서화 |
| 07 | [clean-code](07-clean-code/) | 클린 코드 — 코드 자체가 문서 (Before/After) |
| 08 | [mermaid](08-mermaid/) | Mermaid.js 다이어그램 8종 갤러리 |
| 09 | [inline-comments](09-inline-comments/) | 효과적인 인라인 주석 작성법 |
| 10 | [doxygen](10-doxygen/) | Doxygen — C/C++ 자동 문서 생성 |
| 11 | [redoc](11-redoc/) | ReDoc — 정적 API 문서 (Swagger UI 비교) |
| 12 | [markdown-guide](12-markdown-guide/) | Markdown 문법 + README 템플릿 |

### 인터페이스 & 테스트

| 번호 | 디렉토리 | 설명 |
|------|----------|------|
| 13 | [postman](13-postman/) | Postman Collection + Newman CLI 자동 테스트 |

### 운영 (Operations)

| 번호 | 디렉토리 | 설명 |
|------|----------|------|
| 14 | [runbook](14-runbook/) | 배포, 환경변수, 트러블슈팅, 롤백 가이드 |

## 학습 순서 (권장)

```
Phase 1: 기본 도구
  08-mermaid            → 다이어그램 문법 기초
  12-markdown-guide     → Markdown 문법 + README 작성법

Phase 2: 설계 문서화
  01-architecture       → 전체 시스템 구조 이해
  02-erd                → 데이터 모델 설계
  03-sequence           → 객체 간 흐름 표현

Phase 3: 코드 수준 문서화
  07-clean-code         → 코드 가독성 원칙
  09-inline-comments    → 주석 작성 원칙
  05-jsdoc              → JS 자동 문서화
  06-pydoc              → Python 자동 문서화
  10-doxygen            → C/C++ 자동 문서화

Phase 4: API 문서화
  04-api-documentation  → Swagger UI (인터랙티브 테스트)
  11-redoc              → ReDoc (읽기 좋은 정적 문서)
  13-postman            → API 테스트 자동화

Phase 5: 운영 문서화
  14-runbook            → 배포, 환경변수, 트러블슈팅
```

## 실행 방법 요약

| 모듈 | 실행 명령 | 비고 |
|------|-----------|------|
| 01, 02, 03, 08 | `open index.html` | 브라우저에서 Mermaid 렌더링 |
| 02-erd | `sqlite3 db < schema.sql` | SQLite로 DDL 실행 |
| 04-api-doc | `npm install && npm start` | http://localhost:3000/api-docs |
| 05-jsdoc | `npm install && npm run docs` | HTML 문서 자동 생성 |
| 06-pydoc | `python3 main.py` | `python3 -m pydoc src.calculator` |
| 07-clean-code | `node bad/order-processor.js` vs `node clean/order-processor.js` | Before/After 비교 |
| 09-inline | `node examples.js` | 주석 패턴 데모 |
| 10-doxygen | `doxygen Doxyfile` | `brew install doxygen graphviz` 필요 |
| 11-redoc | `npm install && npm start` | http://localhost:3001 (Swagger vs ReDoc) |
| 13-postman | `npm install && npm test` | Newman CLI 실행 |

## 핵심 원칙

> "주석은 코드로 의도를 표현하지 못했을 때 하는 최후의 수단이다."
> — 로버트 C. 마틴

| 구분 | 주요 내용 | 활용 도구 |
|------|-----------|-----------|
| 개요 | 프로젝트 목적, 기술 스택, 요구사항 | Markdown, Confluence |
| 설계 | 데이터 모델(ERD), 클래스 구조, 워크플로우 | Lucidchart, Mermaid.js |
| 운영 | 배포 프로세스, 환경 변수 설정, 트러블슈팅 | Runbook, Wiki |
| 인터페이스 | API 명세, 데이터 타입, 에러 코드 | Swagger, ReDoc, Postman |
| 코드 | 자동 문서 생성, 클린 코드, 인라인 주석 | JSDoc, Pydoc, Doxygen |
