-- =============================================
-- 샘플 데이터 삽입
-- ERD의 관계가 실제 데이터로 어떻게 표현되는지 확인
-- =============================================

-- 카테고리
INSERT INTO category (name, description) VALUES
    ('전자제품', '스마트폰, 노트북, 태블릿 등'),
    ('의류', '상의, 하의, 아우터 등'),
    ('도서', '프로그래밍, 소설, 자기계발 등');

-- 사용자
INSERT INTO user (email, name, password_hash) VALUES
    ('hong@example.com', '홍길동', 'hashed_pw_001'),
    ('kim@example.com', '김철수', 'hashed_pw_002'),
    ('lee@example.com', '이영희', 'hashed_pw_003');

-- 상품 (category_id로 카테고리 참조 → 1:N 관계)
INSERT INTO product (name, description, price, stock, category_id) VALUES
    ('MacBook Pro 14', 'M3 Pro 칩 탑재', 2990000, 50, 1),
    ('iPhone 15 Pro', 'A17 Pro 칩', 1550000, 200, 1),
    ('Clean Code', '로버트 C. 마틴 저', 33000, 100, 3),
    ('겨울 패딩', '오리털 경량 패딩', 89000, 300, 2);

-- 주문 (user_id로 사용자 참조 → 1:N 관계)
INSERT INTO "order" (user_id, total_amount, status) VALUES
    (1, 3023000, 'paid'),     -- 홍길동: MacBook + Clean Code
    (2, 1550000, 'shipped'),  -- 김철수: iPhone
    (1, 89000, 'pending');    -- 홍길동: 패딩

-- 주문 항목 (order_id, product_id 참조 → 연결 테이블)
INSERT INTO order_item (order_id, product_id, quantity, unit_price) VALUES
    (1, 1, 1, 2990000),  -- 주문1: MacBook 1개
    (1, 3, 1, 33000),    -- 주문1: Clean Code 1권
    (2, 2, 1, 1550000),  -- 주문2: iPhone 1개
    (3, 4, 1, 89000);    -- 주문3: 패딩 1개

-- 리뷰 (user_id, product_id 참조 → 각각 1:N)
INSERT INTO review (user_id, product_id, rating, comment) VALUES
    (1, 1, 5, 'M3 칩 성능이 놀랍습니다. 개발할 때 최고!'),
    (2, 2, 4, '카메라가 정말 좋아요. 다만 배터리가 아쉬워요.'),
    (1, 3, 5, '모든 개발자가 읽어야 할 책입니다.');
