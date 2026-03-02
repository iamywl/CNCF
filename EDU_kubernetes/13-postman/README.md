# 13. Postman — API 테스트 & 문서화

## 개념

Postman은 **API를 테스트하고 문서화하는 통합 플랫폼**입니다.
Collection(컬렉션)이라는 단위로 API 요청을 구조화하고,
자동화된 테스트와 문서 생성 기능을 제공합니다.

## 왜 필요한가?

- **Swagger와 보완 관계**: Swagger는 명세, Postman은 실제 테스트
- **팀 협업**: Collection을 공유하여 팀원이 동일한 API 테스트 환경 사용
- **자동화 테스트**: Pre-request Script, Test Script로 CI/CD 연동
- **환경 변수**: 개발/스테이징/프로덕션 환경을 쉽게 전환

## Postman 핵심 개념

| 개념 | 설명 |
|------|------|
| **Collection** | API 요청의 폴더 (프로젝트 단위) |
| **Request** | 개별 API 호출 (GET, POST 등) |
| **Environment** | 환경 변수 세트 (dev, staging, prod) |
| **Pre-request Script** | 요청 전에 실행되는 JS 코드 |
| **Test Script** | 응답 후 자동 검증하는 JS 코드 |
| **Variables** | `{{variable}}` 형태로 동적 값 삽입 |

## Collection 파일 구조 (JSON)

Postman Collection v2.1 형식은 JSON으로,
버전 관리(Git)에 포함시켜 팀원과 공유할 수 있습니다.

```json
{
  "info": {
    "name": "E-Commerce API",
    "schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json"
  },
  "item": [
    {
      "name": "Products",
      "item": [
        { "name": "상품 목록 조회", "request": { ... } },
        { "name": "상품 등록", "request": { ... } }
      ]
    }
  ]
}
```

## POC 프로젝트

Postman Collection JSON 파일과, Postman 없이도 실행 가능한 Newman(CLI) 예시를 제공합니다.

### 실행 방법

```bash
cd 13-postman

# 방법 1: Postman 앱에서 Import
# Postman 실행 → Import → collections/ecommerce-api.json

# 방법 2: Newman CLI로 실행 (Postman 없이)
npm install
npm test

# 방법 3: 특정 환경으로 실행
npx newman run collections/ecommerce-api.json \
  -e collections/dev-environment.json
```

### 파일 구조

```
13-postman/
├── README.md                          ← 지금 보고 있는 파일
├── package.json
├── collections/
│   ├── ecommerce-api.json             ← Postman Collection (Import 가능)
│   └── dev-environment.json           ← 개발 환경 변수
└── examples/
    └── test-scripts.md                ← Postman Test Script 가이드
```

## Postman Test Script 주요 패턴

### 상태 코드 검증
```javascript
pm.test("상태 코드 200", function() {
    pm.response.to.have.status(200);
});
```

### JSON 응답 검증
```javascript
pm.test("상품 목록이 배열", function() {
    const json = pm.response.json();
    pm.expect(json).to.be.an('array');
    pm.expect(json.length).to.be.above(0);
});
```

### 응답 시간 검증
```javascript
pm.test("응답 시간 500ms 이하", function() {
    pm.expect(pm.response.responseTime).to.be.below(500);
});
```

### 토큰 자동 저장 (Pre-request → 변수)
```javascript
// 로그인 응답에서 토큰 추출하여 환경 변수에 저장
const json = pm.response.json();
pm.environment.set("access_token", json.accessToken);
// 이후 요청에서 {{access_token}}으로 사용
```

## 학습 포인트

1. **Collection = 팀의 자산**: JSON으로 Git에 커밋하여 버전 관리
2. **환경 변수 분리**: 코드 한 줄 안 바꾸고 dev/staging/prod 전환
3. **Newman = CI/CD**: 커맨드라인에서 Collection 실행 → GitHub Actions 연동
4. **Test Script**: 매 요청마다 자동 검증하여 API 회귀 테스트
