/**
 * 장바구니 모듈
 *
 * 쇼핑몰 장바구니의 핵심 기능을 구현합니다.
 * JSDoc의 복합 타입(@typedef), 콜백(@callback), 클래스 문서화를 학습합니다.
 *
 * @module cart
 */

/**
 * 장바구니에 담긴 개별 상품 항목
 * @typedef {Object} CartItem
 * @property {number} productId - 상품 ID
 * @property {string} name - 상품명
 * @property {number} price - 단가
 * @property {number} quantity - 수량
 */

/**
 * 할인 계산 함수의 콜백 타입
 * @callback DiscountCalculator
 * @param {number} totalAmount - 할인 전 총액
 * @returns {number} 할인 금액
 */

/**
 * 장바구니 클래스
 *
 * 상품 추가, 삭제, 수량 변경, 합계 계산 등
 * 장바구니의 전체 생명주기를 관리합니다.
 *
 * @example
 * const cart = new Cart();
 * cart.addItem({ productId: 1, name: 'MacBook', price: 2990000, quantity: 1 });
 * cart.addItem({ productId: 3, name: 'Clean Code', price: 33000, quantity: 2 });
 * console.log(cart.getTotal()); // 3056000
 */
class Cart {
  constructor() {
    /** @type {CartItem[]} */
    this.items = [];
  }

  /**
   * 장바구니에 상품을 추가합니다.
   * 이미 있는 상품이면 수량을 누적합니다.
   *
   * @param {CartItem} item - 추가할 상품 항목
   * @returns {CartItem[]} 업데이트된 장바구니 목록
   */
  addItem(item) {
    const existing = this.items.find(i => i.productId === item.productId);
    if (existing) {
      existing.quantity += item.quantity;
    } else {
      this.items.push({ ...item });
    }
    return this.items;
  }

  /**
   * 장바구니에서 상품을 제거합니다.
   *
   * @param {number} productId - 제거할 상품 ID
   * @returns {boolean} 제거 성공 여부
   */
  removeItem(productId) {
    const idx = this.items.findIndex(i => i.productId === productId);
    if (idx === -1) return false;
    this.items.splice(idx, 1);
    return true;
  }

  /**
   * 특정 상품의 수량을 변경합니다.
   *
   * @param {number} productId - 상품 ID
   * @param {number} quantity - 새로운 수량 (0이면 제거)
   * @returns {boolean} 변경 성공 여부
   * @throws {Error} 수량이 음수일 때
   */
  updateQuantity(productId, quantity) {
    if (quantity < 0) throw new Error('수량은 0 이상이어야 합니다');
    if (quantity === 0) return this.removeItem(productId);

    const item = this.items.find(i => i.productId === productId);
    if (!item) return false;
    item.quantity = quantity;
    return true;
  }

  /**
   * 장바구니 총액을 계산합니다.
   * 선택적으로 할인 함수를 전달하여 할인을 적용할 수 있습니다.
   *
   * @param {DiscountCalculator} [discountFn] - 할인 계산 콜백 (선택)
   * @returns {number} 최종 금액
   * @example
   * // 할인 없이
   * cart.getTotal() // 3056000
   *
   * // 10% 할인 적용
   * cart.getTotal(total => total * 0.1) // 2750400
   */
  getTotal(discountFn) {
    const subtotal = this.items.reduce(
      (sum, item) => sum + item.price * item.quantity,
      0,
    );
    if (discountFn) {
      return subtotal - discountFn(subtotal);
    }
    return subtotal;
  }

  /**
   * 장바구니 요약 정보를 반환합니다.
   *
   * @returns {{ itemCount: number, totalQuantity: number, totalAmount: number }}
   */
  getSummary() {
    return {
      itemCount: this.items.length,
      totalQuantity: this.items.reduce((sum, i) => sum + i.quantity, 0),
      totalAmount: this.getTotal(),
    };
  }
}

module.exports = { Cart };
