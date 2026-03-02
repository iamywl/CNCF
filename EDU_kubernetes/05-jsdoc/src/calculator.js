/**
 * 산술 연산 모듈
 *
 * 기본적인 사칙연산과 고급 수학 함수를 제공합니다.
 * JSDoc의 기본 태그(@param, @returns, @example)를 학습하기 위한 모듈입니다.
 *
 * @module calculator
 * @author EDU Project
 * @version 1.0.0
 */

/**
 * 두 수를 더합니다.
 *
 * @param {number} a - 첫 번째 피연산자
 * @param {number} b - 두 번째 피연산자
 * @returns {number} 두 수의 합
 * @example
 * add(1, 2)    // 3
 * add(-1, 1)   // 0
 * add(0.1, 0.2) // 0.30000000000000004 (부동소수점 주의)
 */
function add(a, b) {
  return a + b;
}

/**
 * 두 수를 나눕니다.
 *
 * @param {number} dividend - 피제수 (나뉨을 당하는 수)
 * @param {number} divisor - 제수 (나누는 수)
 * @returns {number} 나눗셈 결과
 * @throws {Error} divisor가 0일 때 발생
 * @example
 * divide(10, 2)  // 5
 * divide(7, 3)   // 2.3333...
 * divide(10, 0)  // Error: 0으로 나눌 수 없습니다
 */
function divide(dividend, divisor) {
  if (divisor === 0) {
    throw new Error('0으로 나눌 수 없습니다');
  }
  return dividend / divisor;
}

/**
 * 배열의 평균값을 계산합니다.
 *
 * @param {number[]} numbers - 숫자 배열
 * @returns {number} 평균값
 * @throws {Error} 빈 배열일 때 발생
 * @example
 * average([1, 2, 3, 4, 5])  // 3
 * average([10, 20])          // 15
 */
function average(numbers) {
  if (numbers.length === 0) {
    throw new Error('빈 배열의 평균을 구할 수 없습니다');
  }
  const sum = numbers.reduce((acc, n) => acc + n, 0);
  return sum / numbers.length;
}

/**
 * 거듭제곱을 계산합니다.
 *
 * @param {number} base - 밑
 * @param {number} [exponent=2] - 지수 (기본값: 2, 즉 제곱)
 * @returns {number} base^exponent
 * @example
 * power(3)     // 9  (3^2)
 * power(2, 10) // 1024
 */
function power(base, exponent = 2) {
  return Math.pow(base, exponent);
}

/**
 * 수를 지정된 소수점 자릿수로 반올림합니다.
 *
 * 부동소수점 연산의 오차를 처리하기 위해 사용합니다.
 * 예: 0.1 + 0.2 = 0.30000000000000004 → roundTo(0.1 + 0.2, 1) = 0.3
 *
 * @param {number} value - 반올림할 수
 * @param {number} [decimals=2] - 소수점 자릿수 (기본값: 2)
 * @returns {number} 반올림된 수
 * @see {@link https://floating-point-gui.de/ | 부동소수점 가이드}
 * @example
 * roundTo(3.14159, 2)  // 3.14
 * roundTo(0.1 + 0.2, 1) // 0.3
 */
function roundTo(value, decimals = 2) {
  const factor = Math.pow(10, decimals);
  return Math.round(value * factor) / factor;
}

module.exports = { add, divide, average, power, roundTo };
