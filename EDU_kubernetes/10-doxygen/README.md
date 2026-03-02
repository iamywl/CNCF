# 10. Doxygen — C/C++ 자동 문서 생성

## 개념

Doxygen은 **C, C++, Java, Python** 등의 소스코드에 특수 주석을 달면
HTML, PDF, LaTeX 형태의 **API 문서를 자동 생성**해주는 도구입니다.
특히 C/C++ 생태계에서 사실상의 표준 문서화 도구입니다.

## 왜 필요한가?

- C/C++에는 JSDoc이나 Pydoc 같은 내장 문서화가 없음 → Doxygen이 그 역할
- 함수 프로토타입, 구조체, 매크로 등을 **자동 파싱**하여 문서화
- **콜 그래프(Call Graph)** 와 **의존성 다이어그램**을 자동 생성 (Graphviz 연동)
- Linux Kernel, OpenCV, Qt 등 대형 오픈소스 프로젝트에서 사용

## Doxygen 핵심 주석 문법

### JavaDoc 스타일 (/** ... */)
```c
/**
 * @brief 두 정수를 더합니다.
 *
 * @param a 첫 번째 피연산자
 * @param b 두 번째 피연산자
 * @return 두 수의 합
 *
 * @note 오버플로우 검사를 하지 않습니다.
 * @see subtract()
 */
int add(int a, int b);
```

### Qt 스타일 (/*! ... */)
```c
/*!
 * \brief 두 정수를 더합니다.
 * \param a 첫 번째 피연산자
 * \param b 두 번째 피연산자
 * \return 두 수의 합
 */
int add(int a, int b);
```

## 주요 태그

| 태그 | 용도 | 예시 |
|------|------|------|
| `@brief` | 짧은 설명 (한 줄) | `@brief 메모리를 할당합니다` |
| `@param` | 매개변수 설명 | `@param size 할당할 바이트 수` |
| `@return` | 반환값 설명 | `@return 할당된 메모리 포인터` |
| `@note` | 부가 설명 | `@note 스레드 안전하지 않음` |
| `@warning` | 경고 | `@warning NULL 반환 가능` |
| `@deprecated` | 사용 중지 안내 | `@deprecated v3.0에서 제거 예정` |
| `@file` | 파일 설명 | `@file calculator.h` |
| `@struct` | 구조체 설명 | `@struct Point 2D 좌표` |
| `@enum` | 열거형 설명 | `@enum Color 색상 값` |
| `@code` / `@endcode` | 코드 예시 | 예시 코드 블록 |

## POC 프로젝트

C 소스코드에 Doxygen 주석을 작성하고, HTML 문서를 생성합니다.

### 실행 방법

```bash
cd 10-doxygen

# 1. Doxygen 설치 (macOS)
brew install doxygen graphviz

# 2. HTML 문서 생성
doxygen Doxyfile

# 3. 생성된 문서 열기
open docs/html/index.html

# 4. (선택) C 코드 컴파일 및 실행
gcc -o calculator src/calculator.c src/main.c -lm
./calculator
```

### 파일 구조

```
10-doxygen/
├── README.md          ← 지금 보고 있는 파일
├── Doxyfile           ← Doxygen 설정 파일
└── src/
    ├── calculator.h   ← 헤더 (함수 선언 + Doxygen 주석)
    ├── calculator.c   ← 구현
    ├── types.h        ← 구조체/열거형 Doxygen 문서화
    └── main.c         ← 실행 진입점
```

## 학습 포인트

1. **헤더 파일에 문서화**: .h 파일에 @brief, @param, @return을 작성하면 문서와 선언이 한곳에
2. **자동 다이어그램**: Graphviz와 연동하면 콜 그래프, 의존성 그래프 자동 생성
3. **그룹화**: `@defgroup`으로 관련 함수를 그룹으로 묶어 문서 구조화
4. **JSDoc/Pydoc과 비교**: 태그 이름이 유사하여 학습 전이가 쉬움
