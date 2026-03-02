# 02. ERD (Entity Relationship Diagram)

## 개념

ERD는 데이터베이스의 **테이블(엔티티)** 과 그들 사이의 **관계(Relationship)** 를 시각화한 다이어그램입니다.
시스템 설계의 핵심으로, 데이터가 어떻게 구조화되는지를 정의합니다.

## 왜 필요한가?

- DB 스키마 설계의 청사진 역할
- 팀원 간 데이터 구조에 대한 공통 이해 형성
- 정규화/비정규화 결정의 근거 자료

## ERD 핵심 구성 요소

```
Entity (엔티티)     → 테이블: 사각형으로 표현
Attribute (속성)    → 컬럼: 엔티티 안에 나열
Relationship (관계) → FK: 선으로 연결
Cardinality (기수)  → 1:1, 1:N, N:M
```

## 관계 유형

| 표기 | 의미 | 예시 |
|------|------|------|
| `\|\|--\|\|` | 1:1 (일대일) | 사용자 ↔ 프로필 |
| `\|\|--o{` | 1:N (일대다) | 사용자 ↔ 주문 |
| `}o--o{` | N:M (다대다) | 상품 ↔ 카테고리 |

## POC 프로젝트

E-Commerce 시스템의 ERD를 Mermaid와 실제 SQL DDL로 구현합니다.

### 실행 방법

```bash
# 1. 다이어그램 확인 (브라우저)
open index.html

# 2. DDL로 실제 테이블 생성 (SQLite 사용)
sqlite3 ecommerce.db < schema.sql

# 3. 샘플 데이터 삽입 및 조회
sqlite3 ecommerce.db < seed.sql
sqlite3 ecommerce.db < queries.sql
```

### 파일 구조

```
02-erd/
├── README.md      ← 지금 보고 있는 파일
├── index.html     ← ERD 다이어그램 (브라우저)
├── schema.sql     ← DDL (테이블 생성)
├── seed.sql       ← 샘플 데이터
└── queries.sql    ← 관계를 활용한 조회 쿼리
```

## 학습 포인트

1. **정규화**: 데이터 중복을 제거하여 무결성 확보 (1NF → 2NF → 3NF)
2. **외래 키(FK)**: 관계를 코드가 아닌 DB 레벨에서 강제
3. **인덱스 설계**: ERD에서 자주 조인되는 컬럼에 인덱스 추가 고려
