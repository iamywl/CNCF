# Postman Test Script 가이드

Postman의 Test Script는 **API 응답을 자동으로 검증**하는 JavaScript 코드입니다.
매 요청 후 자동 실행되어, 수동 확인 없이 API가 올바르게 동작하는지 검증합니다.

## 1. 기본 검증

### 상태 코드
```javascript
// 정확한 상태 코드
pm.test("200 OK", function() {
    pm.response.to.have.status(200);
});

// 상태 코드 범위
pm.test("성공 응답 (2xx)", function() {
    pm.expect(pm.response.code).to.be.within(200, 299);
});
```

### 응답 시간
```javascript
pm.test("응답 시간 500ms 이하", function() {
    pm.expect(pm.response.responseTime).to.be.below(500);
});
```

### Content-Type
```javascript
pm.test("JSON 응답", function() {
    pm.response.to.have.header("Content-Type", /application\/json/);
});
```

## 2. JSON 응답 검증

### 필드 존재 여부
```javascript
pm.test("필수 필드 존재", function() {
    const json = pm.response.json();
    pm.expect(json).to.have.property("id");
    pm.expect(json).to.have.property("name");
    pm.expect(json).to.have.property("price");
});
```

### 타입 검증
```javascript
pm.test("필드 타입 검증", function() {
    const json = pm.response.json();
    pm.expect(json.id).to.be.a("number");
    pm.expect(json.name).to.be.a("string");
    pm.expect(json.items).to.be.an("array");
});
```

### 값 검증
```javascript
pm.test("가격은 0 이상", function() {
    const json = pm.response.json();
    pm.expect(json.price).to.be.above(0);
});

pm.test("상태값 유효", function() {
    const json = pm.response.json();
    pm.expect(json.status).to.be.oneOf(["pending", "paid", "shipped"]);
});
```

### 배열 검증
```javascript
pm.test("상품 목록 검증", function() {
    const products = pm.response.json();
    pm.expect(products).to.be.an("array");
    pm.expect(products.length).to.be.above(0);

    // 모든 항목에 price 필드 존재
    products.forEach(function(product) {
        pm.expect(product).to.have.property("price");
    });
});
```

## 3. 변수 활용

### 응답에서 변수 추출
```javascript
// 로그인 후 토큰 저장
pm.test("토큰 저장", function() {
    const json = pm.response.json();
    pm.environment.set("access_token", json.accessToken);
});

// 생성된 리소스 ID 저장
pm.test("생성된 ID 저장", function() {
    const json = pm.response.json();
    pm.environment.set("product_id", json.id);
});
```

### Pre-request Script에서 변수 설정
```javascript
// 타임스탬프 생성
pm.environment.set("timestamp", new Date().toISOString());

// 랜덤 데이터 생성
pm.environment.set("random_email",
    `user_${Date.now()}@test.com`);
```

## 4. 실전 패턴: CRUD 테스트 시나리오

### 1단계: 생성 (POST)
```javascript
pm.test("상품 생성 성공", function() {
    pm.response.to.have.status(201);
    const json = pm.response.json();
    pm.environment.set("test_product_id", json.id);
});
```

### 2단계: 조회 (GET) — {{test_product_id}} 사용
```javascript
pm.test("생성된 상품 조회", function() {
    pm.response.to.have.status(200);
    const json = pm.response.json();
    pm.expect(json.name).to.eql("테스트 상품");
});
```

### 3단계: 수정 (PUT)
```javascript
pm.test("상품 수정 성공", function() {
    pm.response.to.have.status(200);
    const json = pm.response.json();
    pm.expect(json.name).to.eql("수정된 상품명");
});
```

### 4단계: 삭제 (DELETE)
```javascript
pm.test("상품 삭제 성공", function() {
    pm.response.to.have.status(200);
});
```

### 5단계: 삭제 확인 (GET → 404)
```javascript
pm.test("삭제된 상품 조회 시 404", function() {
    pm.response.to.have.status(404);
});
```

## 5. Newman CLI 명령어

```bash
# 기본 실행
newman run collection.json

# 환경 변수 지정
newman run collection.json -e dev-environment.json

# 반복 실행 (부하 테스트)
newman run collection.json -n 10

# HTML 리포트 생성
newman run collection.json -r htmlextra

# 특정 폴더만 실행
newman run collection.json --folder "Products"

# CI/CD에서 실패 시 종료 코드 반환
newman run collection.json --bail
```
