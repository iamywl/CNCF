# 04. API 문서화 (Swagger / OpenAPI)

## 개념

OpenAPI Specification(OAS)은 REST API를 **기계가 읽을 수 있는 형식**으로 정의하는 표준입니다.
Swagger UI는 이 정의를 기반으로 **인터랙티브한 API 문서**를 자동 생성합니다.

## 왜 필요한가?

- 프론트엔드/백엔드 개발자 간 **계약(Contract)** 역할
- API를 문서에서 직접 **테스트** 가능 (Try it out)
- 코드와 문서가 **동기화**되어 항상 최신 상태 유지

## OpenAPI 핵심 구조

```yaml
openapi: 3.0.0
info:                    # API 기본 정보
paths:                   # 엔드포인트 정의
  /users:
    get:                 # HTTP Method
      summary:           # 요약
      parameters:        # 쿼리/패스 파라미터
      responses:         # 응답 정의
        200:
          content:
            application/json:
              schema:    # 응답 스키마 (JSON)
components:
  schemas:               # 재사용 가능한 데이터 모델
```

## POC 프로젝트

Express.js + swagger-jsdoc + swagger-ui-express로 실행 가능한 API 서버를 만듭니다.

### 실행 방법

```bash
cd 04-api-documentation

# 의존성 설치
npm install

# 서버 실행
npm start

# 브라우저에서 Swagger UI 확인
# http://localhost:3000/api-docs
```

### 파일 구조

```
04-api-documentation/
├── README.md          ← 지금 보고 있는 파일
├── package.json       ← 프로젝트 설정
├── server.js          ← Express 서버 + Swagger 설정
├── routes/
│   └── products.js    ← 상품 API (JSDoc 주석 → Swagger 자동 생성)
└── openapi.yaml       ← 수동 작성 OpenAPI 명세 (참고용)
```

## Swagger 주석 → 문서 자동 생성 원리

```javascript
/**
 * @swagger
 * /api/products:
 *   get:
 *     summary: 전체 상품 목록 조회
 *     responses:
 *       200:
 *         description: 성공
 */
router.get('/', (req, res) => { ... });
```

코드 위에 `@swagger` 주석을 달면, swagger-jsdoc이 이를 파싱하여
OpenAPI 명세를 자동 생성하고, Swagger UI가 렌더링합니다.

## 학습 포인트

1. **코드와 문서의 동기화**: 주석 기반 자동 생성으로 문서 누락 방지
2. **Try it out**: Swagger UI에서 직접 API 호출 테스트 가능
3. **Schema 정의**: `components/schemas`로 데이터 모델을 한 곳에 정의하고 재사용
4. **응답 코드 명시**: 200, 400, 404, 500 등 모든 응답 시나리오를 문서화
