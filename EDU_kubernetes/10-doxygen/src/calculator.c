/**
 * @file calculator.c
 * @brief calculator.h 의 구현 파일
 */

#include "calculator.h"
#include <stdio.h>
#include <math.h>

/* ─── 기본 사칙연산 ─────────────────────────────────────── */

double add(double a, double b) {
    return a + b;
}

double subtract(double a, double b) {
    return a - b;
}

double multiply(double a, double b) {
    return a * b;
}

double divide(double dividend, double divisor) {
    if (divisor == 0.0) {
        fprintf(stderr, "WARNING: 0으로 나눌 수 없습니다\n");
        return NAN;
    }
    return dividend / divisor;
}

/* ─── 통계 함수 ─────────────────────────────────────────── */

double average(const double *numbers, int length) {
    if (length <= 0 || numbers == NULL) {
        fprintf(stderr, "ERROR: 유효하지 않은 입력\n");
        return NAN;
    }
    double sum = 0.0;
    for (int i = 0; i < length; i++) {
        sum += numbers[i];
    }
    return sum / length;
}

double max_val(const double *numbers, int length) {
    double max = numbers[0];
    for (int i = 1; i < length; i++) {
        if (numbers[i] > max) max = numbers[i];
    }
    return max;
}

double min_val(const double *numbers, int length) {
    double min = numbers[0];
    for (int i = 1; i < length; i++) {
        if (numbers[i] < min) min = numbers[i];
    }
    return min;
}

double clamp(double value, double min_v, double max_v) {
    if (value < min_v) return min_v;
    if (value > max_v) return max_v;
    return value;
}
