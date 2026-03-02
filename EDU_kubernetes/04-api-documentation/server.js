const express = require('express');
const swaggerJsdoc = require('swagger-jsdoc');
const swaggerUi = require('swagger-ui-express');
const productRoutes = require('./routes/products');

const app = express();
app.use(express.json());

// ─── Swagger 설정 ───────────────────────────────────────────
// swagger-jsdoc은 아래 옵션을 기반으로 코드의 @swagger 주석을 파싱하여
// OpenAPI 3.0 명세를 자동 생성합니다.
const swaggerOptions = {
  definition: {
    openapi: '3.0.0',
    info: {
      title: 'E-Commerce API',
      version: '1.0.0',
      description: 'Swagger/OpenAPI 자동 문서화 POC\n\n'
        + '이 API는 교육 목적으로 만들어졌으며,\n'
        + '코드의 @swagger 주석에서 자동 생성됩니다.',
      contact: { name: 'EDU Project' },
    },
    servers: [
      { url: 'http://localhost:3000', description: '로컬 개발 서버' },
    ],
  },
  // @swagger 주석을 스캔할 파일 경로
  apis: ['./routes/*.js'],
};

const swaggerSpec = swaggerJsdoc(swaggerOptions);

// Swagger UI를 /api-docs 경로에 마운트
app.use('/api-docs', swaggerUi.serve, swaggerUi.setup(swaggerSpec, {
  customSiteTitle: 'E-Commerce API Docs',
  customCss: '.swagger-ui .topbar { display: none }',
}));

// OpenAPI JSON 명세를 직접 확인할 수 있는 엔드포인트
app.get('/api-docs.json', (req, res) => {
  res.json(swaggerSpec);
});

// ─── 라우트 등록 ────────────────────────────────────────────
app.use('/api/products', productRoutes);

// ─── 루트 경로 ──────────────────────────────────────────────
app.get('/', (req, res) => {
  res.json({
    message: 'E-Commerce API - Swagger POC',
    docs: 'http://localhost:3000/api-docs',
    spec: 'http://localhost:3000/api-docs.json',
  });
});

// ─── 서버 시작 ──────────────────────────────────────────────
const PORT = 3000;
app.listen(PORT, () => {
  console.log(`Server:     http://localhost:${PORT}`);
  console.log(`Swagger UI: http://localhost:${PORT}/api-docs`);
  console.log(`OpenAPI JSON: http://localhost:${PORT}/api-docs.json`);
});
