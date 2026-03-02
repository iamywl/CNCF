# 07. 클린 코드 (Clean Code) — 코드 자체가 문서가 되게 하라

## 개념

> "주석은 코드로 의도를 표현하지 못했을 때 하는 최후의 수단이다."
> — 로버트 C. 마틴

가장 좋은 문서화는 **코드 자체가 읽기 쉬운 것**입니다.
변수명, 함수명, 클래스 구조만으로 의도를 전달할 수 있다면
별도의 주석이나 문서가 필요 없습니다.

## 클린 코드 핵심 원칙

### 1. 의미 있는 이름 (Meaningful Names)
```
BAD:  const d = new Date();   // d가 뭐지?
GOOD: const createdAt = new Date();
```

### 2. 함수는 하나의 일만 (Single Responsibility)
```
BAD:  function processUserAndSendEmail(user) { ... }
GOOD: function validateUser(user) { ... }
      function sendWelcomeEmail(user) { ... }
```

### 3. 매직 넘버 제거
```
BAD:  if (age >= 19) { ... }     // 19가 뭐지?
GOOD: const LEGAL_DRINKING_AGE = 19;
      if (age >= LEGAL_DRINKING_AGE) { ... }
```

### 4. 부정 조건 피하기
```
BAD:  if (!isNotActive) { ... }  // 이중 부정
GOOD: if (isActive) { ... }
```

### 5. 조기 반환 (Early Return)
```
BAD:  function getPrice(user) {
        if (user) {
          if (user.membership) {
            return user.membership.price;
          }
        }
        return 0;
      }

GOOD: function getPrice(user) {
        if (!user) return 0;
        if (!user.membership) return 0;
        return user.membership.price;
      }
```

## POC 프로젝트

동일한 기능을 "나쁜 코드"와 "클린 코드"로 각각 구현하여 비교합니다.

### 실행 방법

```bash
cd 07-clean-code

# 나쁜 코드 실행
node bad/order-processor.js

# 클린 코드 실행 (동일한 결과, 읽기 쉬운 코드)
node clean/order-processor.js
```

### 파일 구조

```
07-clean-code/
├── README.md                   ← 지금 보고 있는 파일
├── bad/
│   └── order-processor.js      ← 나쁜 예: 읽기 어려운 코드
└── clean/
    └── order-processor.js      ← 좋은 예: 자기 문서화 코드
```

## 학습 포인트

1. **Before/After 비교**: 동일 기능을 두 가지 스타일로 구현하여 차이를 체감
2. **이름이 곧 문서**: 변수명, 함수명만으로 코드의 의도를 파악
3. **작은 함수**: 한 함수가 한 가지 일만 수행하면 이름으로 설명 가능
4. **주석이 필요 없는 코드**: 코드가 명확하면 주석이 필요 없음
