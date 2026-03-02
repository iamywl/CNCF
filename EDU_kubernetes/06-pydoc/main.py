"""Pydoc POC 실행 진입점

각 모듈의 함수를 호출하여 동작을 확인합니다.
python main.py로 실행하세요.
"""

from src.calculator import add, divide, average, clamp
from src.validator import validate_email, validate_password, validate_registration
from src.data_processor import DataProcessor

print("=== Calculator 모듈 ===")
print(f"add(1, 2) = {add(1, 2)}")
print(f"divide(10, 3) = {divide(10, 3)}")
print(f"average([1,2,3,4,5]) = {average([1, 2, 3, 4, 5])}")
print(f"clamp(15, 0, 10) = {clamp(15, 0, 10)}")
print(f"clamp(-5, 0, 10) = {clamp(-5, 0, 10)}")

print("\n=== Validator 모듈 ===")
print(f"validate_email('user@example.com') = {validate_email('user@example.com')}")
print(f"validate_email('invalid') = {validate_email('invalid')}")
print(f"validate_password('weak') = {validate_password('weak')}")
print(f"validate_password('Str0ng!Pass') = {validate_password('Str0ng!Pass')}")

print("\n=== 종합 검증 ===")
result = validate_registration("user@test.com", "Str0ng!Pass", "홍길동")
print(f"유효한 등록: {result}")
result = validate_registration("bad", "weak", "A")
print(f"무효한 등록: {result}")

print("\n=== DataProcessor 모듈 ===")
students = [
    {"name": "Alice", "score": 85, "grade": "B"},
    {"name": "Bob", "score": 92, "grade": "A"},
    {"name": "Charlie", "score": 78, "grade": "C"},
    {"name": "Diana", "score": 95, "grade": "A"},
    {"name": "Eve", "score": 88, "grade": "B"},
]

dp = DataProcessor(students)
print(f"전체 학생 수: {dp.count()}")
print(f"성적 통계: {dp.stats('score')}")

top_students = dp.filter(lambda s: s["score"] >= 90).sort_by("score", reverse=True)
print(f"90점 이상 (내림차순): {top_students.select('name')}")

b_students = dp.filter(lambda s: s["grade"] == "B")
print(f"B등급 학생: {b_students.select('name')}")
print(f"B등급 평균: {b_students.stats('score').average}")
