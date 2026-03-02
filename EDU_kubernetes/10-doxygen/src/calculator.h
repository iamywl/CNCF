/**
 * @file calculator.h
 * @brief 산술 연산 라이브러리
 *
 * 기본적인 사칙연산과 고급 수학 함수를 제공합니다.
 * Doxygen 주석의 기본 사용법을 학습하기 위한 모듈입니다.
 *
 * @author EDU Project
 * @version 1.0.0
 * @date 2024-01-01
 *
 * @par 사용 예시:
 * @code
 *   #include "calculator.h"
 *   double result = add(3.0, 4.0);     // 7.0
 *   double avg = average(arr, 5);       // 배열 평균
 * @endcode
 */

#ifndef CALCULATOR_H
#define CALCULATOR_H

/**
 * @defgroup arithmetic 기본 사칙연산
 * @brief 덧셈, 뺄셈, 곱셈, 나눗셈
 * @{
 */

/**
 * @brief 두 수를 더합니다.
 *
 * @param a 첫 번째 피연산자
 * @param b 두 번째 피연산자
 * @return 두 수의 합 (a + b)
 *
 * @note 오버플로우 검사를 하지 않습니다.
 */
double add(double a, double b);

/**
 * @brief 두 수를 뺍니다.
 *
 * @param a 피감수
 * @param b 감수
 * @return a - b
 */
double subtract(double a, double b);

/**
 * @brief 두 수를 곱합니다.
 *
 * @param a 첫 번째 피연산자
 * @param b 두 번째 피연산자
 * @return a * b
 */
double multiply(double a, double b);

/**
 * @brief 두 수를 나눕니다.
 *
 * @param dividend 피제수 (나뉨을 당하는 수)
 * @param divisor 제수 (나누는 수)
 * @return 나눗셈 결과 (dividend / divisor)
 *
 * @warning divisor가 0이면 NaN을 반환합니다.
 *
 * @par 예시:
 * @code
 *   divide(10.0, 3.0);  // 3.333...
 *   divide(10.0, 0.0);  // NaN (경고 출력)
 * @endcode
 */
double divide(double dividend, double divisor);

/** @} */ // end of arithmetic

/**
 * @defgroup statistics 통계 함수
 * @brief 평균, 최대, 최소, 클램프
 * @{
 */

/**
 * @brief 배열의 평균을 계산합니다.
 *
 * @param[in] numbers 숫자 배열 (읽기 전용)
 * @param[in] length 배열 길이
 * @return 평균값
 *
 * @pre length > 0 이어야 합니다.
 * @pre numbers != NULL 이어야 합니다.
 *
 * @par 예시:
 * @code
 *   double arr[] = {1.0, 2.0, 3.0, 4.0, 5.0};
 *   average(arr, 5);  // 3.0
 * @endcode
 */
double average(const double *numbers, int length);

/**
 * @brief 배열의 최대값을 반환합니다.
 *
 * @param[in] numbers 숫자 배열
 * @param[in] length 배열 길이
 * @return 최대값
 *
 * @pre length > 0
 */
double max_val(const double *numbers, int length);

/**
 * @brief 배열의 최소값을 반환합니다.
 *
 * @param[in] numbers 숫자 배열
 * @param[in] length 배열 길이
 * @return 최소값
 *
 * @pre length > 0
 */
double min_val(const double *numbers, int length);

/**
 * @brief 값을 지정된 범위 내로 제한합니다.
 *
 * min_v <= value <= max_v 범위 밖이면 경계값으로 잘라냅니다.
 *
 * @param value 제한할 값
 * @param min_v 최소 경계
 * @param max_v 최대 경계
 * @return 범위 내로 제한된 값
 *
 * @pre min_v <= max_v
 *
 * @par 예시:
 * @code
 *   clamp(15.0, 0.0, 10.0);  // 10.0
 *   clamp(-5.0, 0.0, 10.0);  // 0.0
 *   clamp(5.0, 0.0, 10.0);   // 5.0
 * @endcode
 */
double clamp(double value, double min_v, double max_v);

/** @} */ // end of statistics

#endif /* CALCULATOR_H */
