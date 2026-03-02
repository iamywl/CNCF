// ============================================================
// 실무에서 자주 마주치는 주석 패턴
// ============================================================
// 실제 프로젝트에서 볼 수 있는 주석 사용 사례를 모았습니다.

// ─── 패턴 1: 정규식 설명 ───────────────────────────────────
// 정규식은 읽기 어려우므로 주석으로 의도를 설명하는 것이 좋습니다.

function isValidKoreanPhoneNumber(phone) {
  // 한국 휴대폰 번호 형식: 010-XXXX-XXXX 또는 01X-XXX-XXXX
  // 010: 현재 사용되는 번호
  // 011, 016, 017, 018, 019: 구형 번호 (하이픈 선택적)
  const pattern = /^01[016789]-?\d{3,4}-?\d{4}$/;
  return pattern.test(phone);
}

// ─── 패턴 2: 매직 넘버에 맥락 부여 ────────────────────────

function calculateRetryDelay(attemptNumber) {
  // 지수 백오프(Exponential Backoff) + 지터(Jitter)
  // - 기본 대기: 1초 × 2^attempt (1s, 2s, 4s, 8s, ...)
  // - 최대 대기: 30초 (서버 과부하 시에도 30초 이상 대기하지 않음)
  // - 지터: ±20% 랜덤 (여러 클라이언트가 동시에 재시도하는 것을 방지)
  // REF: https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/
  const BASE_DELAY_MS = 1000;
  const MAX_DELAY_MS = 30000;
  const JITTER_FACTOR = 0.2;

  const delay = Math.min(BASE_DELAY_MS * Math.pow(2, attemptNumber), MAX_DELAY_MS);
  const jitter = delay * JITTER_FACTOR * (Math.random() * 2 - 1);
  return Math.round(delay + jitter);
}

// ─── 패턴 3: 의도적인 폴스루(Fall-through) ────────────────

function getStatusMessage(statusCode) {
  switch (statusCode) {
    case 200:
      return '성공';
    case 201:
      return '생성됨';

    // 3xx: 리다이렉션 → 모두 동일한 메시지 반환 (의도적 폴스루)
    case 301: // 영구 이동
    case 302: // 임시 이동
    case 307: // 임시 리다이렉트
    case 308: // 영구 리다이렉트
      return '리다이렉션';

    case 400:
      return '잘못된 요청';
    case 401:
      return '인증 필요';
    case 403:
      return '접근 거부';
    case 404:
      return '찾을 수 없음';
    case 429:
      return '요청 과다 (Rate Limit)';

    case 500:
      return '서버 내부 오류';
    case 502:
      return '게이트웨이 오류';
    case 503:
      return '서비스 일시 중단';

    default:
      return `알 수 없는 상태 코드: ${statusCode}`;
  }
}

// ─── 패턴 4: 환경별 분기 설명 ─────────────────────────────

function getApiBaseUrl() {
  const env = process.env.NODE_ENV || 'development';

  // 환경별 API 엔드포인트 (인프라팀 위키: /wiki/api-endpoints 참조)
  // - development: 로컬 Docker Compose
  // - staging: AWS ECS (테스트 데이터)
  // - production: AWS ECS + CloudFront CDN
  const endpoints = {
    development: 'http://localhost:8080',
    staging: 'https://api-staging.example.com',
    production: 'https://api.example.com',
  };

  return endpoints[env] || endpoints.development;
}

// ─── 패턴 5: 성능 최적화 근거 ─────────────────────────────

function findDuplicates(items) {
  // Set을 사용한 O(n) 중복 검출
  // 이전 구현(이중 루프)은 O(n^2)로 10,000건 이상에서 2초 이상 소요.
  // Set으로 교체 후 10,000건 기준 5ms로 개선됨 (2024-02 성능 리포트)
  const seen = new Set();
  const duplicates = new Set();

  for (const item of items) {
    if (seen.has(item.id)) {
      duplicates.add(item.id);
    }
    seen.add(item.id);
  }

  return [...duplicates];
}

// ─── 패턴 6: 외부 API 제약 사항 ──────────────────────────

function formatAddressForDeliveryApi(address) {
  // CJ대한통운 배송 API 제약사항 (API 문서 v3.2 기준):
  // - 주소는 반드시 도로명 주소여야 함 (지번 주소 미지원)
  // - 상세주소는 최대 40바이트 (한글 13자)
  // - 특수문자 중 '|', '~' 사용 불가 (API 내부 구분자로 사용)
  const MAX_DETAIL_LENGTH = 13;

  let detail = address.detail || '';
  detail = detail.replace(/[|~]/g, ' ');
  if (detail.length > MAX_DETAIL_LENGTH) {
    detail = detail.substring(0, MAX_DETAIL_LENGTH);
  }

  return {
    roadAddress: address.road,
    detailAddress: detail,
    zipCode: address.zipCode,
  };
}

// ─── 실행 ──────────────────────────────────────────────────

console.log('=== 실무 주석 패턴 데모 ===\n');

console.log('전화번호 검증:');
console.log('  010-1234-5678:', isValidKoreanPhoneNumber('010-1234-5678'));
console.log('  01012345678:', isValidKoreanPhoneNumber('01012345678'));
console.log('  02-1234-5678:', isValidKoreanPhoneNumber('02-1234-5678'));

console.log('\n재시도 딜레이 (지수 백오프):');
for (let i = 0; i < 6; i++) {
  console.log(`  시도 #${i}: ${calculateRetryDelay(i)}ms`);
}

console.log('\nHTTP 상태 메시지:');
[200, 301, 404, 429, 500].forEach(code => {
  console.log(`  ${code}: ${getStatusMessage(code)}`);
});

console.log('\nAPI Base URL:', getApiBaseUrl());

console.log('\n중복 검출:');
const items = [
  { id: 1, name: 'A' }, { id: 2, name: 'B' },
  { id: 1, name: 'A복제' }, { id: 3, name: 'C' }, { id: 2, name: 'B복제' },
];
console.log('  중복 ID:', findDuplicates(items));

console.log('\n배송 API 주소 포맷:');
console.log('  ', formatAddressForDeliveryApi({
  road: '서울시 강남구 테헤란로 123',
  detail: '4층 개발팀|A구역~옆',
  zipCode: '06234',
}));
