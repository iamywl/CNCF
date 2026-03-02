# 06. Pydoc — Python Docstring & 자동 문서화

## 개념

Python의 **Docstring**은 함수, 클래스, 모듈의 첫 줄에 작성하는 문서화 문자열입니다.
`pydoc`, `Sphinx`, `pdoc` 등의 도구로 HTML 문서를 자동 생성할 수 있습니다.

## 왜 필요한가?

- `help(함수명)`으로 REPL에서 즉시 문서 확인 가능
- IDE가 Docstring을 파싱하여 자동완성 및 타입 힌트 제공
- Sphinx로 공식 문서 수준의 HTML 문서 자동 생성

## Docstring 스타일 비교

### Google Style (권장)
```python
def divide(a: float, b: float) -> float:
    """두 수를 나눕니다.

    Args:
        a: 피제수
        b: 제수 (0이 아니어야 함)

    Returns:
        나눗셈 결과

    Raises:
        ZeroDivisionError: b가 0일 때
    """
```

### NumPy Style
```python
def divide(a, b):
    """두 수를 나눕니다.

    Parameters
    ----------
    a : float
        피제수
    b : float
        제수

    Returns
    -------
    float
        나눗셈 결과
    """
```

### reStructuredText Style (Sphinx 기본)
```python
def divide(a, b):
    """두 수를 나눕니다.

    :param a: 피제수
    :type a: float
    :param b: 제수
    :type b: float
    :returns: 나눗셈 결과
    :rtype: float
    :raises ZeroDivisionError: b가 0일 때
    """
```

## POC 프로젝트

Google Style Docstring으로 문서화된 Python 모듈을 제공합니다.

### 실행 방법

```bash
cd 06-pydoc

# 모듈 실행
python main.py

# pydoc으로 터미널에서 문서 확인
python -m pydoc src.calculator
python -m pydoc src.validator

# pydoc으로 HTML 문서 생성
python -m pydoc -w src.calculator
python -m pydoc -w src.validator
python -m pydoc -w src.data_processor

# 또는 pdoc으로 더 예쁜 HTML 생성 (pip install pdoc 필요)
# pdoc src/ -o docs/
```

### 파일 구조

```
06-pydoc/
├── README.md              ← 지금 보고 있는 파일
├── requirements.txt       ← 의존성 (pdoc은 선택)
├── main.py                ← 실행 진입점
└── src/
    ├── __init__.py
    ├── calculator.py      ← 산술 연산 (기본 Docstring)
    ├── validator.py       ← 유효성 검사 (타입 힌트 + Docstring)
    └── data_processor.py  ← 데이터 처리 (클래스 Docstring, dataclass)
```

## 학습 포인트

1. **Google Style 통일**: 팀 내 Docstring 스타일을 통일하여 일관성 유지
2. **타입 힌트 + Docstring**: Python 3.5+ 타입 힌트와 Docstring을 함께 사용
3. **help() 활용**: REPL에서 `help(함수명)`으로 즉시 문서 확인
4. **자동 생성**: pydoc, pdoc, Sphinx 등으로 HTML 문서 자동 생성
