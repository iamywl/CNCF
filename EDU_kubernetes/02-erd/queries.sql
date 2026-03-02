-- =============================================
-- ERD 관계를 활용한 실전 쿼리 예시
-- 각 쿼리가 ERD의 어떤 관계를 활용하는지 주석으로 설명
-- =============================================

-- [쿼리 1] 사용자별 주문 내역 (User → Order: 1:N)
-- JOIN을 통해 user.id = order.user_id 관계를 타고 이동
SELECT
    u.name AS 사용자,
    o.id AS 주문번호,
    o.total_amount AS 총액,
    o.status AS 상태,
    o.ordered_at AS 주문일시
FROM user u
JOIN "order" o ON u.id = o.user_id
ORDER BY u.name, o.ordered_at;

-- [쿼리 2] 주문 상세 조회 (Order → OrderItem → Product: 1:N:1)
-- 연결 테이블(order_item)을 거쳐 주문에 포함된 상품 정보를 조회
SELECT
    o.id AS 주문번호,
    p.name AS 상품명,
    oi.quantity AS 수량,
    oi.unit_price AS 단가,
    (oi.quantity * oi.unit_price) AS 소계
FROM "order" o
JOIN order_item oi ON o.id = oi.order_id
JOIN product p ON oi.product_id = p.id
WHERE o.id = 1;

-- [쿼리 3] 상품별 평균 평점 (Product → Review: 1:N + 집계)
SELECT
    p.name AS 상품명,
    COUNT(r.id) AS 리뷰수,
    ROUND(AVG(r.rating), 1) AS 평균평점
FROM product p
LEFT JOIN review r ON p.id = r.product_id
GROUP BY p.id
ORDER BY 평균평점 DESC;

-- [쿼리 4] 카테고리별 매출 (Category → Product → OrderItem: 다중 JOIN)
-- ERD의 관계 체인을 따라가며 집계하는 복합 쿼리
SELECT
    c.name AS 카테고리,
    COUNT(DISTINCT oi.order_id) AS 주문건수,
    SUM(oi.quantity * oi.unit_price) AS 총매출
FROM category c
JOIN product p ON c.id = p.category_id
JOIN order_item oi ON p.id = oi.product_id
GROUP BY c.id
ORDER BY 총매출 DESC;

-- [쿼리 5] 홍길동이 리뷰를 남긴 상품 목록 (User → Review → Product)
-- 세 테이블을 연결하여 특정 사용자의 리뷰 상품 조회
SELECT
    p.name AS 상품명,
    r.rating AS 평점,
    r.comment AS 코멘트
FROM user u
JOIN review r ON u.id = r.user_id
JOIN product p ON r.product_id = p.id
WHERE u.name = '홍길동';
