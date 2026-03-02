/**
 * @file types.h
 * @brief 공용 데이터 타입 정의
 *
 * 프로젝트에서 사용하는 구조체, 열거형, 타입 별칭을 정의합니다.
 * Doxygen으로 구조체/열거형을 문서화하는 방법을 학습합니다.
 */

#ifndef TYPES_H
#define TYPES_H

/**
 * @brief 2D 좌표를 나타내는 구조체
 *
 * 평면 위의 한 점을 (x, y) 좌표로 표현합니다.
 *
 * @par 사용 예시:
 * @code
 *   Point p = {3.0, 4.0};
 *   printf("(%f, %f)\n", p.x, p.y);
 * @endcode
 */
typedef struct {
    double x;  /**< X 좌표 */
    double y;  /**< Y 좌표 */
} Point;

/**
 * @brief 2D 벡터를 나타내는 구조체
 *
 * 방향과 크기를 가진 벡터입니다.
 * Point와 동일한 구조이지만 의미가 다릅니다.
 */
typedef struct {
    double dx;  /**< X 방향 성분 */
    double dy;  /**< Y 방향 성분 */
} Vector2D;

/**
 * @brief 색상 열거형
 *
 * 지원하는 색상 목록입니다.
 * 터미널 출력 시 ANSI 코드와 매핑됩니다.
 */
typedef enum {
    COLOR_RED,     /**< 빨강 (ANSI: 31) */
    COLOR_GREEN,   /**< 초록 (ANSI: 32) */
    COLOR_BLUE,    /**< 파랑 (ANSI: 34) */
    COLOR_YELLOW,  /**< 노랑 (ANSI: 33) */
    COLOR_RESET    /**< 색상 초기화 (ANSI: 0) */
} Color;

/**
 * @brief 연산 결과를 나타내는 구조체
 *
 * 연산의 성공/실패와 결과값을 함께 반환할 때 사용합니다.
 * Go 언어의 (value, error) 패턴과 유사합니다.
 *
 * @par 사용 예시:
 * @code
 *   Result r = safe_divide(10.0, 0.0);
 *   if (!r.success) {
 *       printf("Error: %s\n", r.error_msg);
 *   }
 * @endcode
 */
typedef struct {
    double value;          /**< 연산 결과값 */
    int success;           /**< 성공 여부 (1: 성공, 0: 실패) */
    char error_msg[256];   /**< 오류 메시지 (실패 시) */
} Result;

/**
 * @brief 통계 정보를 담는 구조체
 */
typedef struct {
    int count;        /**< 데이터 건수 */
    double sum;       /**< 합계 */
    double average;   /**< 평균 */
    double min;       /**< 최소값 */
    double max;       /**< 최대값 */
} Stats;

#endif /* TYPES_H */
