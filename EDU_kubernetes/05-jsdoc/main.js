/**
 * JSDoc POC 실행 진입점
 *
 * 각 모듈의 함수를 호출하여 동작을 확인합니다.
 * 이 파일을 node main.js로 실행하면 결과를 볼 수 있습니다.
 */

const { add, divide, average, power, roundTo } = require('./src/calculator');
const { validateEmail, validatePassword, validateRegistration } = require('./src/validator');
const { Cart } = require('./src/cart');

console.log('=== Calculator 모듈 ===');
console.log('add(1, 2):', add(1, 2));
console.log('divide(10, 3):', divide(10, 3));
console.log('average([1,2,3,4,5]):', average([1, 2, 3, 4, 5]));
console.log('power(2, 10):', power(2, 10));
console.log('roundTo(0.1 + 0.2, 1):', roundTo(0.1 + 0.2, 1));

console.log('\n=== Validator 모듈 ===');
console.log('validateEmail("user@example.com"):', validateEmail('user@example.com'));
console.log('validateEmail("invalid"):', validateEmail('invalid'));
console.log('validatePassword("weak"):', validatePassword('weak'));
console.log('validatePassword("Str0ng!Pass"):', validatePassword('Str0ng!Pass'));

console.log('\n=== Cart 모듈 ===');
const cart = new Cart();
cart.addItem({ productId: 1, name: 'MacBook Pro', price: 2990000, quantity: 1 });
cart.addItem({ productId: 3, name: 'Clean Code', price: 33000, quantity: 2 });
console.log('장바구니 항목:', cart.items);
console.log('총액:', cart.getTotal());
console.log('10% 할인 적용:', cart.getTotal(total => total * 0.1));
console.log('요약:', cart.getSummary());
