// ============================================================
// BAD CODE EXAMPLE - 나쁜 코드의 전형적인 패턴들
// ============================================================
// 이 코드는 "동작은 하지만 읽기 어려운" 코드의 예시입니다.
// clean/ 디렉토리의 동일 기능 코드와 비교해 보세요.

// 의미 없는 변수명
function proc(d) {
  let r = [];
  for (let i = 0; i < d.length; i++) {
    let t = 0;
    for (let j = 0; j < d[i].items.length; j++) {
      // 매직 넘버: 0.1이 뭐지? 10000이 뭐지?
      if (d[i].items[j].p * d[i].items[j].q > 10000) {
        t += d[i].items[j].p * d[i].items[j].q * 0.9;
      } else {
        t += d[i].items[j].p * d[i].items[j].q;
      }
    }
    // 복잡한 조건문 (이중 부정, 중첩)
    if (d[i].s !== 'cancelled') {
      if (d[i].m === true) {
        // 멤버십이면 5% 추가 할인
        t = t * 0.95;
      }
      if (t > 50000) {
        // 5만원 이상이면 무료배송
        d[i].ship = 0;
      } else {
        d[i].ship = 3000;
      }
      t += d[i].ship;
      // tax
      let tx = t * 0.1;
      d[i].total = t + tx;
      d[i].tax = tx;
      r.push(d[i]);
    }
  }
  // 정렬
  r.sort(function(a, b) { return b.total - a.total; });
  return r;
}

// 테스트 데이터
const data = [
  {
    id: 1, m: true, s: 'active',
    items: [
      { n: 'MacBook', p: 2990000, q: 1 },
      { n: 'Mouse', p: 89000, q: 2 },
    ],
  },
  {
    id: 2, m: false, s: 'active',
    items: [
      { n: 'Book', p: 15000, q: 3 },
    ],
  },
  {
    id: 3, m: true, s: 'cancelled',
    items: [
      { n: 'Keyboard', p: 150000, q: 1 },
    ],
  },
];

const result = proc(data);
console.log('=== BAD CODE 결과 ===');
result.forEach(o => {
  console.log(`주문 #${o.id}: 총액 ${o.total}, 세금 ${o.tax}, 배송비 ${o.ship}`);
});
