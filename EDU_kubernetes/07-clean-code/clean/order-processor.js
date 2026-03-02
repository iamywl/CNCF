// ============================================================
// CLEAN CODE EXAMPLE - 코드 자체가 문서가 되는 코드
// ============================================================
// bad/ 디렉토리의 동일 기능 코드와 비교해 보세요.
// 주석 없이도 코드의 의도를 파악할 수 있습니다.

// ─── 상수: 매직 넘버를 의미 있는 이름으로 ─────────────────
const BULK_DISCOUNT_THRESHOLD = 10000;
const BULK_DISCOUNT_RATE = 0.10;
const MEMBERSHIP_DISCOUNT_RATE = 0.05;
const FREE_SHIPPING_THRESHOLD = 50000;
const STANDARD_SHIPPING_FEE = 3000;
const TAX_RATE = 0.10;

// ─── 핵심 함수: 한 가지 일만 수행 ─────────────────────────

function calculateItemSubtotal(item) {
  const subtotal = item.price * item.quantity;
  const hasBulkDiscount = subtotal > BULK_DISCOUNT_THRESHOLD;

  return hasBulkDiscount
    ? subtotal * (1 - BULK_DISCOUNT_RATE)
    : subtotal;
}

function calculateOrderSubtotal(items) {
  return items.reduce(
    (total, item) => total + calculateItemSubtotal(item),
    0,
  );
}

function applyMembershipDiscount(amount, isMember) {
  return isMember
    ? amount * (1 - MEMBERSHIP_DISCOUNT_RATE)
    : amount;
}

function calculateShippingFee(amount) {
  return amount >= FREE_SHIPPING_THRESHOLD
    ? 0
    : STANDARD_SHIPPING_FEE;
}

function calculateTax(amount) {
  return amount * TAX_RATE;
}

function isActiveOrder(order) {
  return order.status !== 'cancelled';
}

// ─── 주문 처리 파이프라인 ──────────────────────────────────

function processOrder(order) {
  const subtotal = calculateOrderSubtotal(order.items);
  const discountedAmount = applyMembershipDiscount(subtotal, order.isMember);
  const shippingFee = calculateShippingFee(discountedAmount);
  const amountBeforeTax = discountedAmount + shippingFee;
  const tax = calculateTax(amountBeforeTax);

  return {
    ...order,
    shippingFee,
    tax,
    total: amountBeforeTax + tax,
  };
}

function processOrders(orders) {
  return orders
    .filter(isActiveOrder)
    .map(processOrder)
    .sort((a, b) => b.total - a.total);
}

// ─── 출력 ──────────────────────────────────────────────────

function formatCurrency(amount) {
  return amount.toLocaleString('ko-KR') + '원';
}

function printOrderSummary(order) {
  console.log(
    `주문 #${order.id}: ` +
    `총액 ${formatCurrency(order.total)}, ` +
    `세금 ${formatCurrency(order.tax)}, ` +
    `배송비 ${formatCurrency(order.shippingFee)}`,
  );
}

// ─── 실행 ──────────────────────────────────────────────────

const orders = [
  {
    id: 1, isMember: true, status: 'active',
    items: [
      { name: 'MacBook', price: 2990000, quantity: 1 },
      { name: 'Mouse', price: 89000, quantity: 2 },
    ],
  },
  {
    id: 2, isMember: false, status: 'active',
    items: [
      { name: 'Book', price: 15000, quantity: 3 },
    ],
  },
  {
    id: 3, isMember: true, status: 'cancelled',
    items: [
      { name: 'Keyboard', price: 150000, quantity: 1 },
    ],
  },
];

console.log('=== CLEAN CODE 결과 ===');
const processedOrders = processOrders(orders);
processedOrders.forEach(printOrderSummary);
