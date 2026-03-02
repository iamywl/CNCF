"""유효성 검사 모듈

사용자 입력값의 유효성을 검증하는 함수들을 제공합니다.
타입 힌트와 Docstring을 함께 사용하는 방법을 학습합니다.

Example:
    >>> from src.validator import validate_email
    >>> result = validate_email("user@example.com")
    >>> result
    ValidationResult(valid=True, errors=[])
"""

import re
from dataclasses import dataclass, field
from typing import List, Optional


@dataclass
class ValidationResult:
    """유효성 검사 결과를 담는 데이터 클래스

    Attributes:
        valid: 유효 여부 (True면 통과)
        errors: 오류 메시지 리스트 (유효하면 빈 리스트)

    Example:
        >>> result = ValidationResult(valid=True, errors=[])
        >>> result.valid
        True
    """
    valid: bool
    errors: List[str] = field(default_factory=list)


def validate_email(email: str) -> ValidationResult:
    """이메일 주소의 유효성을 검사합니다.

    RFC 5322를 완벽히 구현하지는 않지만,
    일반적인 이메일 형식을 검증하기에 충분합니다.

    Args:
        email: 검사할 이메일 주소

    Returns:
        ValidationResult: 검사 결과

    Example:
        >>> validate_email("user@example.com")
        ValidationResult(valid=True, errors=[])
        >>> validate_email("invalid")
        ValidationResult(valid=False, errors=['올바른 이메일 형식이 아닙니다'])
    """
    errors = []
    pattern = r'^[^\s@]+@[^\s@]+\.[^\s@]+$'

    if not email or not isinstance(email, str):
        errors.append("이메일은 필수 입력값입니다")
    elif not re.match(pattern, email):
        errors.append("올바른 이메일 형식이 아닙니다")

    return ValidationResult(valid=len(errors) == 0, errors=errors)


def validate_password(password: str) -> ValidationResult:
    """비밀번호의 강도를 검사합니다.

    보안 정책:
        - 최소 8자 이상
        - 영문 대문자 1개 이상
        - 영문 소문자 1개 이상
        - 숫자 1개 이상
        - 특수문자 1개 이상

    Args:
        password: 검사할 비밀번호

    Returns:
        ValidationResult: 검사 결과 (위반 항목이 errors에 포함)

    Example:
        >>> validate_password("Str0ng!Pass")
        ValidationResult(valid=True, errors=[])
        >>> result = validate_password("weak")
        >>> result.valid
        False
    """
    errors = []
    rules = [
        (lambda p: len(p) >= 8, "8자 이상이어야 합니다"),
        (lambda p: bool(re.search(r'[A-Z]', p)), "영문 대문자를 1개 이상 포함해야 합니다"),
        (lambda p: bool(re.search(r'[a-z]', p)), "영문 소문자를 1개 이상 포함해야 합니다"),
        (lambda p: bool(re.search(r'[0-9]', p)), "숫자를 1개 이상 포함해야 합니다"),
        (lambda p: bool(re.search(r'[!@#$%^&*(),.?":{}|<>]', p)), "특수문자를 1개 이상 포함해야 합니다"),
    ]

    for check, message in rules:
        if not check(password or ""):
            errors.append(message)

    return ValidationResult(valid=len(errors) == 0, errors=errors)


def validate_registration(
    email: str,
    password: str,
    name: str,
    age: Optional[int] = None,
) -> ValidationResult:
    """사용자 등록 정보를 종합 검증합니다.

    이메일, 비밀번호, 이름을 각각 검증하고
    모든 오류를 하나의 결과로 합칩니다.

    Args:
        email: 이메일 주소
        password: 비밀번호
        name: 사용자 이름 (2~20자)
        age: 나이 (선택, 0 이상)

    Returns:
        ValidationResult: 종합 검사 결과

    Example:
        >>> validate_registration("user@test.com", "Str0ng!Pass", "홍길동")
        ValidationResult(valid=True, errors=[])
    """
    errors = []

    errors.extend(validate_email(email).errors)
    errors.extend(validate_password(password).errors)

    if not name or len(name) < 2 or len(name) > 20:
        errors.append("이름은 2~20자여야 합니다")

    if age is not None and (not isinstance(age, int) or age < 0):
        errors.append("나이는 0 이상의 정수여야 합니다")

    return ValidationResult(valid=len(errors) == 0, errors=errors)
