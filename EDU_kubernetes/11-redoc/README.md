# 11. ReDoc — OpenAPI 정적 API 문서 생성

## 개념

ReDoc은 OpenAPI(Swagger) 명세 파일로부터 **아름답고 읽기 쉬운 정적 API 문서**를
생성하는 도구입니다. Swagger UI가 "인터랙티브 테스트"에 초점을 맞춘다면,
ReDoc은 **"읽기 좋은 문서"** 에 초점을 맞춥니다.

## Swagger UI vs ReDoc 비교

| 항목 | Swagger UI | ReDoc |
|------|-----------|-------|
| 주 목적 | API 테스트 (Try it out) | API 문서 읽기 |
| 레이아웃 | 상하 스크롤 | 3단 레이아웃 (메뉴/설명/코드) |
| 코드 예시 | 제한적 | 다국어 코드 샘플 지원 |
| 검색 | 없음 | 전체 텍스트 검색 |
| 배포 | 서버 필요 | 정적 HTML 1개 파일 |
| 사용 사례 | 개발 중 테스트 | 외부 공개용 문서 |

## POC 프로젝트

동일한 OpenAPI 명세를 Swagger UI와 ReDoc으로 각각 렌더링하여 비교합니다.

### 실행 방법

```bash
cd 11-redoc

# 방법 1: 정적 HTML 직접 열기 (서버 불필요)
open index.html

# 방법 2: Node.js 서버로 Swagger UI와 ReDoc 동시 비교
npm install
npm start
# http://localhost:3001/redoc   ← ReDoc
# http://localhost:3001/swagger ← Swagger UI (비교용)
```

### 파일 구조

```
11-redoc/
├── README.md          ← 지금 보고 있는 파일
├── index.html         ← ReDoc 정적 HTML (서버 없이 브라우저에서 열기)
├── openapi.yaml       ← OpenAPI 3.0 명세 (상세 버전)
├── package.json       ← Node.js 서버 (Swagger UI + ReDoc 비교)
└── server.js          ← 비교 서버
```

## OpenAPI 명세 작성 팁 (ReDoc에서 잘 보이게)

1. **description에 Markdown 사용**: ReDoc은 설명 필드의 Markdown을 완전 렌더링
2. **x-codeSamples 확장**: 각 엔드포인트에 다국어 코드 샘플 추가 가능
3. **tags로 그룹화**: 좌측 메뉴가 tags 기준으로 생성됨
4. **examples 활용**: 요청/응답 예시를 넣으면 우측 패널에 표시

## 학습 포인트

1. **정적 배포**: HTML 1개 파일로 어디서든 API 문서 호스팅 가능
2. **Swagger와 역할 분리**: 개발 중은 Swagger UI, 외부 공개는 ReDoc
3. **OpenAPI 명세가 핵심**: 도구는 바뀌어도 명세만 잘 작성하면 어디서든 활용
4. **3단 레이아웃**: 좌측 메뉴 + 중앙 설명 + 우측 코드 예시
