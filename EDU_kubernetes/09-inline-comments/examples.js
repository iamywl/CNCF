// ============================================================
// 인라인 주석 가이드 — 좋은 주석 vs 나쁜 주석
// ============================================================
// 이 파일은 주석의 '가치'를 이해하기 위한 교육용 코드입니다.
// node examples.js로 실행하면 각 함수의 결과를 확인할 수 있습니다.

// ─── 나쁜 주석 예시 ────────────────────────────────────────

// BAD: 코드를 그대로 읽어준 주석 (가치 없음)
function badExample1() {
  // 가격을 계산한다
  const price = 1000;

  // 할인율을 적용한다
  const discount = 0.1;

  // 최종 가격을 반환한다
  return price * (1 - discount);
}

// BAD: 거짓 주석 (코드와 불일치하여 오히려 혼란)
function badExample2(items) {
  // 상품 가격의 합계를 구한다 (실제로는 평균을 구하고 있음)
  const total = items.reduce((sum, item) => sum + item.price, 0);
  return total / items.length; // <-- 이건 합계가 아닌 평균!
}

// BAD: 주석으로 나쁜 이름을 보상하려는 시도
function calc(a, b, c) {
  // a: 상품 가격
  // b: 수량
  // c: 할인율
  return a * b * (1 - c);
}
// GOOD: 이름을 고치면 주석이 필요 없다
function calculateDiscountedTotal(price, quantity, discountRate) {
  return price * quantity * (1 - discountRate);
}

// ─── 좋은 주석 예시 ────────────────────────────────────────

// GOOD: WHY — 비직관적인 결정의 이유를 설명
function sortOrdersByDate(orders) {
  // 힙 정렬(Heap Sort) 사용: 일반적으로 퀵 정렬이 빠르지만,
  // 이미 거의 정렬된 주문 데이터에서는 퀵 정렬이 O(n^2)로 퇴화함.
  // 실측 결과 힙 정렬이 3배 빠름 (2024-03 성능 테스트)
  return heapSort(orders, 'date');
}
function heapSort(arr, key) { return [...arr].sort((a, b) => a[key] - b[key]); }

// GOOD: WARNING — 위험한 코드 영역을 경고
function processPayment(order) {
  // WARNING: acquireLock → charge → releaseLock 순서를 반드시 유지할 것.
  // charge 전에 releaseLock을 호출하면 동시 결제로 이중 과금 발생.
  // 이슈 #1234에서 실제 발생하여 수정됨.
  acquireLock(order.id);
  try {
    charge(order);
  } finally {
    releaseLock(order.id);
  }
}
function acquireLock(id) { console.log(`  Lock acquired: ${id}`); }
function releaseLock(id) { console.log(`  Lock released: ${id}`); }
function charge(order) { console.log(`  Charged: ${order.amount}`); }

// GOOD: HACK — 임시 우회 코드임을 명시
function getUserAge(user) {
  // HACK: 외부 인증 시스템(AuthProvider v2.3)이 birthDate를
  // ISO 형식이 아닌 "YYYYMMDD" 문자열로 반환하는 버그가 있음.
  // AuthProvider v3.0 업그레이드 후 이 파싱 로직을 제거할 것.
  // 관련 이슈: AUTH-5678
  const birthStr = user.birthDate; // "19900315" 형태
  const year = parseInt(birthStr.substring(0, 4));
  const now = new Date().getFullYear();
  return now - year;
}

// GOOD: TODO — 향후 개선 사항을 추적
function getProductReviews(productId) {
  // TODO(2024-Q3): 현재 N+1 쿼리 문제 있음.
  // DataLoader 패턴으로 배치 쿼리로 전환 필요.
  // 현재 100개 이하 상품에서만 호출되어 성능 문제 없으나,
  // 상품 수 증가 시 반드시 개선해야 함.
  const reviews = [
    { id: 1, productId, rating: 5, text: '최고!' },
    { id: 2, productId, rating: 4, text: '좋아요' },
  ];
  return reviews;
}

// GOOD: 비즈니스 규칙 설명
function calculateShippingFee(orderAmount, region) {
  // 비즈니스 규칙 (2024-01 마케팅팀 요청):
  // - 5만원 이상: 무료 배송
  // - 제주/도서산간: 추가 3,000원 (무료 배송 대상이어도 부과)
  // - 기본 배송비: 3,000원
  const FREE_SHIPPING_THRESHOLD = 50000;
  const BASE_FEE = 3000;
  const REMOTE_SURCHARGE = 3000;
  const REMOTE_REGIONS = ['jeju', 'island'];

  let fee = orderAmount >= FREE_SHIPPING_THRESHOLD ? 0 : BASE_FEE;

  if (REMOTE_REGIONS.includes(region)) {
    fee += REMOTE_SURCHARGE;
  }

  return fee;
}

// ─── 실행 ──────────────────────────────────────────────────

console.log('=== 나쁜 주석 예시 (결과는 동일하지만 코드 가독성 차이) ===');
console.log('badExample1():', badExample1());
console.log('calc(1000, 2, 0.1):', calc(1000, 2, 0.1));
console.log('calculateDiscountedTotal(1000, 2, 0.1):', calculateDiscountedTotal(1000, 2, 0.1));

console.log('\n=== 좋은 주석 예시 ===');
console.log('getUserAge({ birthDate: "19900315" }):', getUserAge({ birthDate: '19900315' }));
console.log('getProductReviews(1):', getProductReviews(1));
console.log('calculateShippingFee(60000, "seoul"):', calculateShippingFee(60000, 'seoul'));
console.log('calculateShippingFee(60000, "jeju"):', calculateShippingFee(60000, 'jeju'));
console.log('calculateShippingFee(30000, "seoul"):', calculateShippingFee(30000, 'seoul'));

console.log('\n=== WARNING 주석이 있는 결제 처리 ===');
processPayment({ id: 'order-42', amount: 50000 });
