-- =============================================
-- E-Commerce ERD → DDL (SQLite)
-- =============================================
-- 이 파일은 ERD에서 정의한 엔티티를 실제 SQL 테이블로 변환한 것입니다.
-- ERD의 관계(1:N, N:M)가 FOREIGN KEY로 어떻게 구현되는지 확인하세요.

-- 사용자 테이블
CREATE TABLE IF NOT EXISTS user (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    email       TEXT    NOT NULL UNIQUE,    -- UK: 이메일 중복 방지
    name        TEXT    NOT NULL,
    password_hash TEXT  NOT NULL,
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- 카테고리 테이블
CREATE TABLE IF NOT EXISTS category (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL,
    description TEXT
);

-- 상품 테이블 (Category와 1:N 관계)
CREATE TABLE IF NOT EXISTS product (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL,
    description TEXT,
    price       REAL    NOT NULL CHECK(price >= 0),
    stock       INTEGER NOT NULL DEFAULT 0 CHECK(stock >= 0),
    category_id INTEGER NOT NULL,
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (category_id) REFERENCES category(id)
);

-- 주문 테이블 (User와 1:N 관계)
CREATE TABLE IF NOT EXISTS "order" (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL,
    total_amount REAL    NOT NULL DEFAULT 0,
    status       TEXT    NOT NULL DEFAULT 'pending'
                         CHECK(status IN ('pending', 'paid', 'shipped', 'delivered', 'cancelled')),
    ordered_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES user(id)
);

-- 주문 항목 테이블 (Order, Product와 각각 1:N 관계)
-- 이 테이블이 Order와 Product 사이의 N:M 관계를 해소하는 연결 테이블 역할
CREATE TABLE IF NOT EXISTS order_item (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    order_id    INTEGER NOT NULL,
    product_id  INTEGER NOT NULL,
    quantity    INTEGER NOT NULL CHECK(quantity > 0),
    unit_price  REAL    NOT NULL,  -- 주문 시점의 가격을 기록 (상품 가격 변동 대비)
    FOREIGN KEY (order_id)   REFERENCES "order"(id),
    FOREIGN KEY (product_id) REFERENCES product(id)
);

-- 리뷰 테이블 (User, Product와 각각 1:N 관계)
CREATE TABLE IF NOT EXISTS review (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL,
    product_id  INTEGER NOT NULL,
    rating      INTEGER NOT NULL CHECK(rating BETWEEN 1 AND 5),
    comment     TEXT,
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id)    REFERENCES user(id),
    FOREIGN KEY (product_id) REFERENCES product(id)
);

-- 인덱스: 자주 조회되는 외래 키 컬럼에 생성
CREATE INDEX IF NOT EXISTS idx_product_category ON product(category_id);
CREATE INDEX IF NOT EXISTS idx_order_user ON "order"(user_id);
CREATE INDEX IF NOT EXISTS idx_order_item_order ON order_item(order_id);
CREATE INDEX IF NOT EXISTS idx_review_product ON review(product_id);
