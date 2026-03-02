# 05. JSDoc — JavaScript 자동 문서 생성

## 개념

JSDoc은 JavaScript 소스코드에 **정해진 규칙의 주석**을 달면,
HTML 형태의 **API 문서를 자동 생성**해주는 도구입니다.

## 왜 필요한가?

- 코드를 읽지 않아도 함수의 **입출력과 용도**를 파악 가능
- IDE(VSCode)가 JSDoc을 파싱하여 **자동완성과 타입 힌트** 제공
- `jsdoc` CLI로 HTML 문서를 자동 생성 → 항상 코드와 동기화

## JSDoc 핵심 태그

| 태그 | 용도 | 예시 |
|------|------|------|
| `@param` | 매개변수 설명 | `@param {string} name - 사용자 이름` |
| `@returns` | 반환값 설명 | `@returns {boolean} 유효 여부` |
| `@typedef` | 커스텀 타입 정의 | `@typedef {Object} User` |
| `@example` | 사용 예시 코드 | `@example add(1, 2) // 3` |
| `@throws` | 예외 상황 | `@throws {Error} ID가 없을 때` |
| `@deprecated` | 사용 중지 안내 | `@deprecated v2.0에서 제거 예정` |
| `@see` | 관련 참조 | `@see {@link https://...}` |
| `@module` | 모듈 설명 | `@module utils/validator` |

## POC 프로젝트

실제 유틸리티 모듈을 JSDoc으로 문서화하고 HTML을 생성합니다.

### 실행 방법

```bash
cd 05-jsdoc

# 의존성 설치
npm install

# JSDoc으로 HTML 문서 생성
npm run docs

# 생성된 문서 열기
open docs/index.html

# 실행 확인
npm start
```

### 파일 구조

```
05-jsdoc/
├── README.md          ← 지금 보고 있는 파일
├── package.json
├── jsdoc.json         ← JSDoc 설정 파일
├── src/
│   ├── calculator.js  ← 산술 연산 모듈 (기본 태그 학습)
│   ├── validator.js   ← 유효성 검사 모듈 (typedef, throws 학습)
│   └── cart.js        ← 장바구니 모듈 (복합 타입, callback 학습)
└── main.js            ← 실행 진입점
```

## 학습 포인트

1. **@param + 타입**: `{string}`, `{number}`, `{Object}`, `{string[]}` 등 타입 명시
2. **@typedef**: 복합 객체 타입을 별도 정의하여 재사용
3. **@example**: 사용 예시로 함수의 의도를 명확히 전달
4. **IDE 연동**: VSCode에서 JSDoc 기반 자동완성을 직접 체험
