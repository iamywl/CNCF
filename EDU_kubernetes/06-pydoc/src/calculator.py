"""산술 연산 모듈

기본적인 사칙연산과 통계 함수를 제공합니다.
Google Style Docstring의 기본 사용법을 학습할 수 있습니다.

Example:
    >>> from src.calculator import add, divide
    >>> add(1, 2)
    3
    >>> divide(10, 3)
    3.3333333333333335
"""

from typing import List, Optional


def add(a: float, b: float) -> float:
    """두 수를 더합니다.

    Args:
        a: 첫 번째 피연산자
        b: 두 번째 피연산자

    Returns:
        두 수의 합

    Example:
        >>> add(1, 2)
        3
        >>> add(-1, 1)
        0
    """
    return a + b


def subtract(a: float, b: float) -> float:
    """두 수를 뺍니다.

    Args:
        a: 피감수
        b: 감수

    Returns:
        a - b의 결과
    """
    return a - b


def divide(dividend: float, divisor: float) -> float:
    """두 수를 나눕니다.

    Args:
        dividend: 피제수 (나뉨을 당하는 수)
        divisor: 제수 (나누는 수, 0이 아니어야 함)

    Returns:
        나눗셈 결과

    Raises:
        ZeroDivisionError: divisor가 0일 때 발생

    Example:
        >>> divide(10, 2)
        5.0
        >>> divide(7, 3)
        2.3333333333333335
    """
    if divisor == 0:
        raise ZeroDivisionError("0으로 나눌 수 없습니다")
    return dividend / divisor


def average(numbers: List[float]) -> float:
    """숫자 리스트의 평균을 계산합니다.

    Args:
        numbers: 숫자 리스트 (빈 리스트 불가)

    Returns:
        평균값

    Raises:
        ValueError: 빈 리스트가 전달되었을 때

    Example:
        >>> average([1, 2, 3, 4, 5])
        3.0
        >>> average([10, 20])
        15.0
    """
    if not numbers:
        raise ValueError("빈 리스트의 평균을 구할 수 없습니다")
    return sum(numbers) / len(numbers)


def clamp(value: float, min_val: float, max_val: float) -> float:
    """값을 지정된 범위 내로 제한합니다.

    min_val <= value <= max_val 범위를 벗어나면
    경계값으로 잘라냅니다.

    Args:
        value: 제한할 값
        min_val: 최소값
        max_val: 최대값

    Returns:
        범위 내로 제한된 값

    Raises:
        ValueError: min_val이 max_val보다 클 때

    Example:
        >>> clamp(15, 0, 10)
        10
        >>> clamp(-5, 0, 10)
        0
        >>> clamp(5, 0, 10)
        5
    """
    if min_val > max_val:
        raise ValueError(f"min_val({min_val})이 max_val({max_val})보다 큽니다")
    return max(min_val, min(value, max_val))
