/**
 * 유효성 검사 모듈
 *
 * 사용자 입력값의 유효성을 검증하는 함수들을 제공합니다.
 * JSDoc의 고급 태그(@typedef, @throws, @property)를 학습하기 위한 모듈입니다.
 *
 * @module validator
 */

/**
 * 유효성 검사 결과를 나타내는 타입
 * @typedef {Object} ValidationResult
 * @property {boolean} valid - 유효 여부
 * @property {string[]} errors - 오류 메시지 배열 (유효하면 빈 배열)
 */

/**
 * 이메일 주소의 유효성을 검사합니다.
 *
 * RFC 5322를 완벽히 구현하지는 않지만,
 * 일반적인 이메일 형식을 검증하기에 충분합니다.
 *
 * @param {string} email - 검사할 이메일 주소
 * @returns {ValidationResult} 검사 결과
 * @example
 * validateEmail('user@example.com')
 * // { valid: true, errors: [] }
 *
 * validateEmail('invalid-email')
 * // { valid: false, errors: ['올바른 이메일 형식이 아닙니다'] }
 */
function validateEmail(email) {
  const errors = [];
  const emailRegex = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

  if (!email || typeof email !== 'string') {
    errors.push('이메일은 필수 입력값입니다');
  } else if (!emailRegex.test(email)) {
    errors.push('올바른 이메일 형식이 아닙니다');
  }

  return { valid: errors.length === 0, errors };
}

/**
 * 비밀번호의 강도를 검사합니다.
 *
 * 보안 정책:
 * - 최소 8자 이상
 * - 영문 대문자 1개 이상
 * - 영문 소문자 1개 이상
 * - 숫자 1개 이상
 * - 특수문자 1개 이상
 *
 * @param {string} password - 검사할 비밀번호
 * @returns {ValidationResult} 검사 결과
 * @example
 * validatePassword('Str0ng!Pass')
 * // { valid: true, errors: [] }
 *
 * validatePassword('weak')
 * // { valid: false, errors: ['8자 이상이어야 합니다', ...] }
 */
function validatePassword(password) {
  const errors = [];

  if (!password || password.length < 8) {
    errors.push('8자 이상이어야 합니다');
  }
  if (!/[A-Z]/.test(password)) {
    errors.push('영문 대문자를 1개 이상 포함해야 합니다');
  }
  if (!/[a-z]/.test(password)) {
    errors.push('영문 소문자를 1개 이상 포함해야 합니다');
  }
  if (!/[0-9]/.test(password)) {
    errors.push('숫자를 1개 이상 포함해야 합니다');
  }
  if (!/[!@#$%^&*(),.?":{}|<>]/.test(password)) {
    errors.push('특수문자를 1개 이상 포함해야 합니다');
  }

  return { valid: errors.length === 0, errors };
}

/**
 * 사용자 등록 정보를 나타내는 타입
 * @typedef {Object} UserRegistration
 * @property {string} email - 이메일 주소
 * @property {string} password - 비밀번호
 * @property {string} name - 사용자 이름 (2~20자)
 * @property {number} [age] - 나이 (선택, 0 이상)
 */

/**
 * 사용자 등록 정보를 종합 검증합니다.
 *
 * 이메일, 비밀번호, 이름을 각각 검증하고
 * 모든 오류를 하나의 결과로 합칩니다.
 *
 * @param {UserRegistration} user - 등록할 사용자 정보
 * @returns {ValidationResult} 종합 검사 결과
 * @example
 * validateRegistration({
 *   email: 'user@example.com',
 *   password: 'Str0ng!Pass',
 *   name: '홍길동'
 * })
 * // { valid: true, errors: [] }
 */
function validateRegistration(user) {
  const errors = [];

  // 이메일 검증
  const emailResult = validateEmail(user.email);
  errors.push(...emailResult.errors);

  // 비밀번호 검증
  const passwordResult = validatePassword(user.password);
  errors.push(...passwordResult.errors);

  // 이름 검증
  if (!user.name || user.name.length < 2 || user.name.length > 20) {
    errors.push('이름은 2~20자여야 합니다');
  }

  // 나이 검증 (선택 필드)
  if (user.age !== undefined && (typeof user.age !== 'number' || user.age < 0)) {
    errors.push('나이는 0 이상의 숫자여야 합니다');
  }

  return { valid: errors.length === 0, errors };
}

module.exports = { validateEmail, validatePassword, validateRegistration };
