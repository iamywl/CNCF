"""데이터 처리 모듈

클래스 기반의 데이터 처리를 구현합니다.
클래스 Docstring, dataclass, 메서드 체이닝 문서화를 학습합니다.

Example:
    >>> from src.data_processor import DataProcessor
    >>> dp = DataProcessor([{"name": "Alice", "score": 85}, {"name": "Bob", "score": 92}])
    >>> dp.filter(lambda r: r["score"] >= 90).count()
    1
"""

from dataclasses import dataclass
from typing import Any, Callable, Dict, List, Optional


@dataclass
class Stats:
    """통계 정보를 담는 데이터 클래스

    Attributes:
        count: 데이터 건수
        total: 합계
        average: 평균
        min_val: 최소값
        max_val: 최대값
    """
    count: int
    total: float
    average: float
    min_val: float
    max_val: float


class DataProcessor:
    """데이터 처리기

    딕셔너리 리스트 형태의 데이터를 필터링, 정렬, 변환, 집계합니다.
    메서드 체이닝을 지원하여 파이프라인 형태로 데이터를 처리할 수 있습니다.

    Attributes:
        data: 처리할 데이터 (딕셔너리 리스트)

    Example:
        학생 성적 데이터를 처리하는 예시::

            students = [
                {"name": "Alice", "score": 85, "grade": "B"},
                {"name": "Bob", "score": 92, "grade": "A"},
                {"name": "Charlie", "score": 78, "grade": "C"},
                {"name": "Diana", "score": 95, "grade": "A"},
            ]

            dp = DataProcessor(students)

            # 90점 이상 학생만 필터링 후 이름 추출
            top_students = (
                dp.filter(lambda s: s["score"] >= 90)
                  .sort_by("score", reverse=True)
                  .select("name")
            )
            # ["Diana", "Bob"]
    """

    def __init__(self, data: List[Dict[str, Any]]) -> None:
        """DataProcessor를 초기화합니다.

        Args:
            data: 처리할 딕셔너리 리스트
        """
        self._data = list(data)

    def filter(self, predicate: Callable[[Dict], bool]) -> "DataProcessor":
        """조건에 맞는 레코드만 필터링합니다.

        메서드 체이닝을 위해 새로운 DataProcessor를 반환합니다.

        Args:
            predicate: 각 레코드를 받아 bool을 반환하는 함수

        Returns:
            필터링된 새 DataProcessor

        Example:
            >>> dp.filter(lambda r: r["score"] >= 90)
        """
        return DataProcessor([r for r in self._data if predicate(r)])

    def sort_by(self, key: str, reverse: bool = False) -> "DataProcessor":
        """특정 키로 정렬합니다.

        Args:
            key: 정렬 기준 키
            reverse: True이면 내림차순 (기본: 오름차순)

        Returns:
            정렬된 새 DataProcessor

        Example:
            >>> dp.sort_by("score", reverse=True)
        """
        sorted_data = sorted(self._data, key=lambda r: r.get(key, 0), reverse=reverse)
        return DataProcessor(sorted_data)

    def select(self, key: str) -> List[Any]:
        """특정 키의 값만 추출합니다.

        Args:
            key: 추출할 키

        Returns:
            해당 키의 값 리스트

        Example:
            >>> dp.select("name")
            ["Alice", "Bob", "Charlie"]
        """
        return [r.get(key) for r in self._data]

    def stats(self, key: str) -> Stats:
        """특정 숫자 키에 대한 통계를 계산합니다.

        Args:
            key: 통계를 계산할 숫자 키

        Returns:
            Stats: 통계 정보 (건수, 합계, 평균, 최소, 최대)

        Raises:
            ValueError: 데이터가 비어있을 때

        Example:
            >>> dp.stats("score")
            Stats(count=4, total=350.0, average=87.5, min_val=78.0, max_val=95.0)
        """
        values = [r[key] for r in self._data if key in r]
        if not values:
            raise ValueError(f"'{key}' 키에 해당하는 데이터가 없습니다")
        return Stats(
            count=len(values),
            total=sum(values),
            average=sum(values) / len(values),
            min_val=min(values),
            max_val=max(values),
        )

    def count(self) -> int:
        """현재 데이터 건수를 반환합니다.

        Returns:
            데이터 건수
        """
        return len(self._data)

    def to_list(self) -> List[Dict[str, Any]]:
        """처리된 데이터를 리스트로 반환합니다.

        Returns:
            딕셔너리 리스트
        """
        return list(self._data)
