/**
 * @file main.c
 * @brief Doxygen POC 실행 진입점
 *
 * calculator.h의 함수들을 호출하여 동작을 확인합니다.
 */

#include <stdio.h>
#include "calculator.h"
#include "types.h"

/**
 * @brief 프로그램 진입점
 *
 * 각 모듈의 함수를 호출하고 결과를 출력합니다.
 *
 * @return 0 (정상 종료)
 */
int main(void) {
    printf("=== 기본 사칙연산 ===\n");
    printf("add(3, 4)      = %.1f\n", add(3.0, 4.0));
    printf("subtract(10, 3)= %.1f\n", subtract(10.0, 3.0));
    printf("multiply(5, 6) = %.1f\n", multiply(5.0, 6.0));
    printf("divide(10, 3)  = %.4f\n", divide(10.0, 3.0));
    printf("divide(10, 0)  = %.1f\n", divide(10.0, 0.0));

    printf("\n=== 통계 함수 ===\n");
    double arr[] = {1.0, 2.0, 3.0, 4.0, 5.0};
    int len = 5;
    printf("average = %.1f\n", average(arr, len));
    printf("max     = %.1f\n", max_val(arr, len));
    printf("min     = %.1f\n", min_val(arr, len));

    printf("\n=== clamp ===\n");
    printf("clamp(15, 0, 10) = %.1f\n", clamp(15.0, 0.0, 10.0));
    printf("clamp(-5, 0, 10) = %.1f\n", clamp(-5.0, 0.0, 10.0));
    printf("clamp(5, 0, 10)  = %.1f\n", clamp(5.0, 0.0, 10.0));

    printf("\n=== 구조체 타입 ===\n");
    Point p = {3.0, 4.0};
    printf("Point: (%.1f, %.1f)\n", p.x, p.y);

    Stats s = {5, 15.0, 3.0, 1.0, 5.0};
    printf("Stats: count=%d, avg=%.1f, min=%.1f, max=%.1f\n",
           s.count, s.average, s.min, s.max);

    return 0;
}
