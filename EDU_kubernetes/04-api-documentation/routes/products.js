const express = require('express');
const router = express.Router();

// ─── 인메모리 데이터 (DB 대체) ──────────────────────────────
let products = [
  { id: 1, name: 'MacBook Pro 14', price: 2990000, stock: 50, category: '전자제품' },
  { id: 2, name: 'iPhone 15 Pro', price: 1550000, stock: 200, category: '전자제품' },
  { id: 3, name: 'Clean Code', price: 33000, stock: 100, category: '도서' },
  { id: 4, name: '겨울 패딩', price: 89000, stock: 300, category: '의류' },
];
let nextId = 5;

// ─────────────────────────────────────────────────────────────
// 아래의 @swagger 주석이 Swagger UI 문서를 자동 생성합니다.
// 주석의 YAML 구조 = OpenAPI 3.0 명세
// ─────────────────────────────────────────────────────────────

/**
 * @swagger
 * components:
 *   schemas:
 *     Product:
 *       type: object
 *       required:
 *         - name
 *         - price
 *       properties:
 *         id:
 *           type: integer
 *           description: 상품 고유 ID (자동 생성)
 *           example: 1
 *         name:
 *           type: string
 *           description: 상품명
 *           example: "MacBook Pro 14"
 *         price:
 *           type: number
 *           description: 가격 (원)
 *           example: 2990000
 *         stock:
 *           type: integer
 *           description: 재고 수량
 *           example: 50
 *         category:
 *           type: string
 *           description: 카테고리
 *           example: "전자제품"
 *     ProductInput:
 *       type: object
 *       required:
 *         - name
 *         - price
 *       properties:
 *         name:
 *           type: string
 *           example: "Galaxy S24"
 *         price:
 *           type: number
 *           example: 1350000
 *         stock:
 *           type: integer
 *           example: 100
 *         category:
 *           type: string
 *           example: "전자제품"
 *     Error:
 *       type: object
 *       properties:
 *         error:
 *           type: string
 *           example: "상품을 찾을 수 없습니다"
 */

/**
 * @swagger
 * tags:
 *   - name: Products
 *     description: 상품 관리 API
 */

/**
 * @swagger
 * /api/products:
 *   get:
 *     tags: [Products]
 *     summary: 전체 상품 목록 조회
 *     description: 등록된 모든 상품을 조회합니다. category 파라미터로 필터링 가능.
 *     parameters:
 *       - in: query
 *         name: category
 *         schema:
 *           type: string
 *         description: 카테고리로 필터링 (예 - 전자제품, 도서, 의류)
 *     responses:
 *       200:
 *         description: 상품 목록
 *         content:
 *           application/json:
 *             schema:
 *               type: array
 *               items:
 *                 $ref: '#/components/schemas/Product'
 */
router.get('/', (req, res) => {
  const { category } = req.query;
  if (category) {
    return res.json(products.filter(p => p.category === category));
  }
  res.json(products);
});

/**
 * @swagger
 * /api/products/{id}:
 *   get:
 *     tags: [Products]
 *     summary: 상품 상세 조회
 *     description: ID로 특정 상품의 상세 정보를 조회합니다.
 *     parameters:
 *       - in: path
 *         name: id
 *         required: true
 *         schema:
 *           type: integer
 *         description: 상품 ID
 *     responses:
 *       200:
 *         description: 상품 상세 정보
 *         content:
 *           application/json:
 *             schema:
 *               $ref: '#/components/schemas/Product'
 *       404:
 *         description: 상품을 찾을 수 없음
 *         content:
 *           application/json:
 *             schema:
 *               $ref: '#/components/schemas/Error'
 */
router.get('/:id', (req, res) => {
  const product = products.find(p => p.id === parseInt(req.params.id));
  if (!product) {
    return res.status(404).json({ error: '상품을 찾을 수 없습니다' });
  }
  res.json(product);
});

/**
 * @swagger
 * /api/products:
 *   post:
 *     tags: [Products]
 *     summary: 새 상품 등록
 *     description: 새로운 상품을 등록합니다. name과 price는 필수입니다.
 *     requestBody:
 *       required: true
 *       content:
 *         application/json:
 *           schema:
 *             $ref: '#/components/schemas/ProductInput'
 *     responses:
 *       201:
 *         description: 등록된 상품
 *         content:
 *           application/json:
 *             schema:
 *               $ref: '#/components/schemas/Product'
 *       400:
 *         description: 필수 필드 누락
 *         content:
 *           application/json:
 *             schema:
 *               $ref: '#/components/schemas/Error'
 */
router.post('/', (req, res) => {
  const { name, price, stock, category } = req.body;
  if (!name || price === undefined) {
    return res.status(400).json({ error: 'name과 price는 필수입니다' });
  }
  const product = { id: nextId++, name, price, stock: stock || 0, category: category || '' };
  products.push(product);
  res.status(201).json(product);
});

/**
 * @swagger
 * /api/products/{id}:
 *   put:
 *     tags: [Products]
 *     summary: 상품 정보 수정
 *     description: ID로 특정 상품의 정보를 수정합니다.
 *     parameters:
 *       - in: path
 *         name: id
 *         required: true
 *         schema:
 *           type: integer
 *         description: 상품 ID
 *     requestBody:
 *       required: true
 *       content:
 *         application/json:
 *           schema:
 *             $ref: '#/components/schemas/ProductInput'
 *     responses:
 *       200:
 *         description: 수정된 상품
 *         content:
 *           application/json:
 *             schema:
 *               $ref: '#/components/schemas/Product'
 *       404:
 *         description: 상품을 찾을 수 없음
 */
router.put('/:id', (req, res) => {
  const idx = products.findIndex(p => p.id === parseInt(req.params.id));
  if (idx === -1) {
    return res.status(404).json({ error: '상품을 찾을 수 없습니다' });
  }
  products[idx] = { ...products[idx], ...req.body };
  res.json(products[idx]);
});

/**
 * @swagger
 * /api/products/{id}:
 *   delete:
 *     tags: [Products]
 *     summary: 상품 삭제
 *     description: ID로 특정 상품을 삭제합니다.
 *     parameters:
 *       - in: path
 *         name: id
 *         required: true
 *         schema:
 *           type: integer
 *         description: 상품 ID
 *     responses:
 *       200:
 *         description: 삭제 성공
 *         content:
 *           application/json:
 *             schema:
 *               type: object
 *               properties:
 *                 message:
 *                   type: string
 *                   example: "삭제되었습니다"
 *       404:
 *         description: 상품을 찾을 수 없음
 */
router.delete('/:id', (req, res) => {
  const idx = products.findIndex(p => p.id === parseInt(req.params.id));
  if (idx === -1) {
    return res.status(404).json({ error: '상품을 찾을 수 없습니다' });
  }
  products.splice(idx, 1);
  res.json({ message: '삭제되었습니다' });
});

module.exports = router;
