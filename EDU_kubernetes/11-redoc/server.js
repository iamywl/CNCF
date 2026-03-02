const express = require('express');
const swaggerUi = require('swagger-ui-express');
const YAML = require('yamljs');
const path = require('path');

const app = express();
const spec = YAML.load(path.join(__dirname, 'openapi.yaml'));

// ─── Swagger UI (비교용) ─────────────────────────────────
app.use('/swagger', swaggerUi.serve, swaggerUi.setup(spec, {
  customSiteTitle: 'Swagger UI - 비교용',
}));

// ─── ReDoc ───────────────────────────────────────────────
app.get('/redoc', (req, res) => {
  res.send(`
    <!DOCTYPE html>
    <html>
    <head>
      <title>E-Commerce API — ReDoc</title>
      <meta charset="utf-8"/>
      <meta name="viewport" content="width=device-width, initial-scale=1">
    </head>
    <body>
      <redoc spec-url='/openapi.json'></redoc>
      <script src="https://cdn.redoc.ly/redoc/latest/bundles/redoc.standalone.js"></script>
    </body>
    </html>
  `);
});

// ─── OpenAPI JSON 엔드포인트 ─────────────────────────────
app.get('/openapi.json', (req, res) => {
  res.json(spec);
});

// ─── 루트: 안내 페이지 ───────────────────────────────────
app.get('/', (req, res) => {
  res.send(`
    <html>
    <head><title>ReDoc vs Swagger UI</title></head>
    <body style="font-family:sans-serif; max-width:600px; margin:50px auto;">
      <h1>ReDoc vs Swagger UI 비교</h1>
      <p>동일한 OpenAPI 명세를 두 가지 도구로 렌더링합니다.</p>
      <ul>
        <li><a href="/redoc" style="font-size:20px;">ReDoc</a> — 읽기 좋은 3단 레이아웃</li>
        <li><a href="/swagger" style="font-size:20px;">Swagger UI</a> — 인터랙티브 테스트</li>
        <li><a href="/openapi.json">OpenAPI JSON</a> — 원본 명세</li>
      </ul>
    </body>
    </html>
  `);
});

const PORT = 3001;
app.listen(PORT, () => {
  console.log(`비교 서버: http://localhost:${PORT}`);
  console.log(`  ReDoc:      http://localhost:${PORT}/redoc`);
  console.log(`  Swagger UI: http://localhost:${PORT}/swagger`);
});
